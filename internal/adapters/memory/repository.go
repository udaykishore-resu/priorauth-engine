// Package memory provides in-memory adapters for every port so the service
// runs with zero infrastructure and tests never need Docker.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
	"github.com/udaykishore-resu/priorauth-engine/internal/ports"
)

// Repository is a mutex-guarded map-backed event store + projection.
type Repository struct {
	mu      sync.RWMutex
	streams map[string][]workflow.Envelope
	views   map[string][]byte // JSON documents, like the Postgres jsonb column
	idem    map[string]string
}

// NewRepository returns an empty repository.
func NewRepository() *Repository {
	return &Repository{streams: map[string][]workflow.Envelope{}, views: map[string][]byte{}, idem: map[string]string{}}
}

// Save implements ports.Repository.
func (m *Repository) Save(_ context.Context, r *workflow.Request, envs []workflow.Envelope) error {
	if len(envs) == 0 {
		return nil
	}
	doc, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("memory: marshal view: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stream := m.streams[r.ID]
	expected := r.Version - len(envs)
	if len(stream) != expected {
		return fmt.Errorf("%w: request %s at version %d, expected %d", ports.ErrConcurrency, r.ID, len(stream), expected)
	}
	for i := range envs {
		if envs[i].ID == "" {
			envs[i].ID = fmt.Sprintf("%s-%d", r.ID, envs[i].Seq)
		}
	}
	m.streams[r.ID] = append(stream, envs...)
	m.views[r.ID] = doc
	return nil
}

// Load implements ports.Repository.
func (m *Repository) Load(_ context.Context, id string) (*workflow.Request, error) {
	m.mu.RLock()
	stream := m.streams[id]
	m.mu.RUnlock()
	if len(stream) == 0 {
		return nil, workflow.ErrNotFound
	}
	return workflow.Load(stream)
}

// Get implements ports.Repository.
func (m *Repository) Get(_ context.Context, id string) (*workflow.Request, error) {
	m.mu.RLock()
	doc, ok := m.views[id]
	m.mu.RUnlock()
	if !ok {
		return nil, workflow.ErrNotFound
	}
	var r workflow.Request
	if err := json.Unmarshal(doc, &r); err != nil {
		return nil, fmt.Errorf("memory: decode view %s: %w", id, err)
	}
	return &r, nil
}

// List implements ports.Repository.
func (m *Repository) List(_ context.Context, f ports.Filter) ([]*workflow.Request, error) {
	m.mu.RLock()
	docs := make([][]byte, 0, len(m.views))
	for _, d := range m.views {
		docs = append(docs, d)
	}
	m.mu.RUnlock()
	var out []*workflow.Request
	for _, d := range docs {
		var r workflow.Request
		if err := json.Unmarshal(d, &r); err != nil {
			return nil, fmt.Errorf("memory: decode view: %w", err)
		}
		if Matches(f, &r) {
			out = append(out, &r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// Matches applies a filter to a request; shared with other adapters' tests.
func Matches(f ports.Filter, r *workflow.Request) bool {
	if f.State != "" && r.State != f.State {
		return false
	}
	if f.Payer != "" && r.Intake.Payer.ID != f.Payer {
		return false
	}
	if f.Active && r.State.Terminal() {
		return false
	}
	return true
}

// Events implements ports.Repository.
func (m *Repository) Events(_ context.Context, id string) ([]workflow.Envelope, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stream, ok := m.streams[id]
	if !ok {
		return nil, workflow.ErrNotFound
	}
	out := make([]workflow.Envelope, len(stream))
	copy(out, stream)
	return out, nil
}

// Reserve implements ports.Repository.
func (m *Repository) Reserve(_ context.Context, key, id string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.idem[key]; ok {
		return existing, false, nil
	}
	m.idem[key] = id
	return id, true, nil
}

// Ping implements ports.Repository.
func (m *Repository) Ping(context.Context) error { return nil }

// Publisher records published envelopes; tests inspect them.
type Publisher struct {
	mu   sync.Mutex
	sent []workflow.Envelope
}

// NewPublisher returns an empty publisher.
func NewPublisher() *Publisher { return &Publisher{} }

// Publish implements ports.Publisher.
func (p *Publisher) Publish(_ context.Context, envs []workflow.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, envs...)
	return nil
}

// Close implements ports.Publisher.
func (p *Publisher) Close() error { return nil }

// Sent returns a copy of everything published so far.
func (p *Publisher) Sent() []workflow.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]workflow.Envelope, len(p.sent))
	copy(out, p.sent)
	return out
}

var (
	_ ports.Repository = (*Repository)(nil)
	_ ports.Publisher  = (*Publisher)(nil)
)
