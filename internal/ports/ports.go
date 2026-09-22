// Package ports declares the interfaces the application depends on for
// persistence, messaging and payer connectivity. Every port has an
// in-memory adapter (used by tests and `make run`) and a real adapter.
package ports

import (
	"context"
	"errors"
	"time"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
)

// ErrConcurrency is returned when an append races another writer.
var ErrConcurrency = errors.New("concurrent modification")

// Filter narrows List.
type Filter struct {
	State  workflow.State
	Payer  string
	Active bool // non-terminal only
	Limit  int
}

// Repository is the event store plus its projection. Save is atomic: the
// events and the projected read-model document commit together, so a
// crash can never leave a request whose view disagrees with its stream.
type Repository interface {
	// Save appends envs to the request's stream and upserts the projection.
	// The expected stream version is r.Version-len(envs); a mismatch returns
	// ErrConcurrency.
	Save(ctx context.Context, r *workflow.Request, envs []workflow.Envelope) error
	// Load rebuilds the aggregate from its events (source of truth).
	Load(ctx context.Context, id string) (*workflow.Request, error)
	// Get reads the projected document.
	Get(ctx context.Context, id string) (*workflow.Request, error)
	// List reads projected documents matching the filter, newest first.
	List(ctx context.Context, f Filter) ([]*workflow.Request, error)
	// Events returns the raw stream for audit and replay.
	Events(ctx context.Context, id string) ([]workflow.Envelope, error)
	// Reserve atomically claims an idempotency key for id. When the key is
	// already taken it returns the existing request id and created=false.
	Reserve(ctx context.Context, key, id string) (existing string, created bool, err error)
	// Ping checks the dependency for readiness.
	Ping(ctx context.Context) error
}

// Publisher emits committed events to downstream consumers.
type Publisher interface {
	Publish(ctx context.Context, envs []workflow.Envelope) error
	Close() error
}

// PayerGateway sends PAS bundles to a payer and inquires about pended ones.
type PayerGateway interface {
	// Submit posts a PAS request Bundle and returns the ClaimResponse JSON.
	Submit(ctx context.Context, bundle []byte) ([]byte, error)
	// Inquire fetches the current ClaimResponse for a payer reference.
	Inquire(ctx context.Context, payerRef string) ([]byte, error)
	// Ping checks connectivity for readiness.
	Ping(ctx context.Context) error
}

// Clock abstracts time for deterministic tests.
type Clock func() time.Time
