// Package postgres implements ports.Repository on PostgreSQL with pgx: an
// append-only request_events table (source of truth), a request_views
// projection kept in the same transaction, and an idempotency_keys table.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
	"github.com/udaykishore-resu/priorauth-engine/internal/ports"
	"github.com/udaykishore-resu/priorauth-engine/migrations"
)

// Repository is the pgx-backed repository.
type Repository struct {
	pool *pgxpool.Pool
}

// Open connects and verifies the pool.
func Open(ctx context.Context, dsn string) (*Repository, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	if cfg.MaxConns < 4 {
		cfg.MaxConns = 8
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &Repository{pool: pool}, nil
}

// Close releases the pool.
func (p *Repository) Close() { p.pool.Close() }

// Ping implements ports.Repository.
func (p *Repository) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// Migrate applies embedded migrations that have not been applied yet, in
// lexical order, each in its own transaction, guarded by an advisory lock
// so concurrent replicas do not race.
func (p *Repository) Migrate(ctx context.Context) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("postgres: acquire: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(727401)`); err != nil {
		return fmt.Errorf("postgres: lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(727401)`) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("postgres: migrations table: %w", err)
	}
	applied := map[string]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("postgres: read migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("postgres: list migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if applied[name] {
			continue
		}
		sql, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("postgres: begin %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("postgres: apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("postgres: record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("postgres: commit %s: %w", name, err)
		}
	}
	return nil
}

// Save implements ports.Repository. The UNIQUE (request_id, seq) constraint
// is the optimistic-concurrency guard: a racing writer's first insert
// conflicts and the whole transaction rolls back.
func (p *Repository) Save(ctx context.Context, r *workflow.Request, envs []workflow.Envelope) error {
	if len(envs) == 0 {
		return nil
	}
	doc, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("postgres: marshal view: %w", err)
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, e := range envs {
		id := e.ID
		if id == "" {
			id = fmt.Sprintf("%s-%d", e.RequestID, e.Seq)
		}
		batch.Queue(`INSERT INTO request_events (id, request_id, seq, type, at, actor, trace_id, payload) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			id, e.RequestID, e.Seq, e.Type, e.At, nullable(e.Actor), nullable(e.TraceID), []byte(e.Payload))
	}
	batch.Queue(`INSERT INTO request_views (id, state, payer, version, updated_at, doc) VALUES ($1,$2,$3,$4,$5,$6)
	             ON CONFLICT (id) DO UPDATE SET state=EXCLUDED.state, payer=EXCLUDED.payer, version=EXCLUDED.version, updated_at=EXCLUDED.updated_at, doc=EXCLUDED.doc
	             WHERE request_views.version < EXCLUDED.version`,
		r.ID, string(r.State), r.Intake.Payer.ID, r.Version, r.UpdatedAt, doc)
	br := tx.SendBatch(ctx, batch)
	for i := 0; i < batch.Len(); i++ {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
				return fmt.Errorf("%w: request %s", ports.ErrConcurrency, r.ID)
			}
			return fmt.Errorf("postgres: save %s: %w", r.ID, err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("postgres: batch close: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Events implements ports.Repository.
func (p *Repository) Events(ctx context.Context, id string) ([]workflow.Envelope, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, request_id, seq, type, at, COALESCE(actor,''), COALESCE(trace_id,''), payload FROM request_events WHERE request_id=$1 ORDER BY seq`, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: events %s: %w", id, err)
	}
	defer rows.Close()
	var out []workflow.Envelope
	for rows.Next() {
		var e workflow.Envelope
		var payload []byte
		if err := rows.Scan(&e.ID, &e.RequestID, &e.Seq, &e.Type, &e.At, &e.Actor, &e.TraceID, &payload); err != nil {
			return nil, fmt.Errorf("postgres: scan event: %w", err)
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, workflow.ErrNotFound
	}
	return out, nil
}

// Load implements ports.Repository.
func (p *Repository) Load(ctx context.Context, id string) (*workflow.Request, error) {
	envs, err := p.Events(ctx, id)
	if err != nil {
		return nil, err
	}
	return workflow.Load(envs)
}

// Get implements ports.Repository.
func (p *Repository) Get(ctx context.Context, id string) (*workflow.Request, error) {
	var doc []byte
	err := p.pool.QueryRow(ctx, `SELECT doc FROM request_views WHERE id=$1`, id).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, workflow.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get %s: %w", id, err)
	}
	var r workflow.Request
	if err := json.Unmarshal(doc, &r); err != nil {
		return nil, fmt.Errorf("postgres: decode view %s: %w", id, err)
	}
	return &r, nil
}

// List implements ports.Repository.
func (p *Repository) List(ctx context.Context, f ports.Filter) ([]*workflow.Request, error) {
	var where []string
	var args []any
	if f.State != "" {
		args = append(args, string(f.State))
		where = append(where, fmt.Sprintf("state=$%d", len(args)))
	}
	if f.Payer != "" {
		args = append(args, f.Payer)
		where = append(where, fmt.Sprintf("payer=$%d", len(args)))
	}
	if f.Active {
		where = append(where, "state NOT IN ('approved','closed')")
	}
	q := `SELECT doc FROM request_views`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY updated_at DESC, id"
	if f.Limit > 0 {
		args = append(args, f.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: list: %w", err)
	}
	defer rows.Close()
	var out []*workflow.Request
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var r workflow.Request
		if err := json.Unmarshal(doc, &r); err != nil {
			return nil, fmt.Errorf("postgres: decode view: %w", err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// Reserve implements ports.Repository.
func (p *Repository) Reserve(ctx context.Context, key, id string) (string, bool, error) {
	tag, err := p.pool.Exec(ctx, `INSERT INTO idempotency_keys (key, request_id) VALUES ($1,$2) ON CONFLICT (key) DO NOTHING`, key, id)
	if err != nil {
		return "", false, fmt.Errorf("postgres: reserve: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return id, true, nil
	}
	var existing string
	if err := p.pool.QueryRow(ctx, `SELECT request_id FROM idempotency_keys WHERE key=$1`, key).Scan(&existing); err != nil {
		return "", false, fmt.Errorf("postgres: lookup key: %w", err)
	}
	return existing, false, nil
}

var _ ports.Repository = (*Repository)(nil)
