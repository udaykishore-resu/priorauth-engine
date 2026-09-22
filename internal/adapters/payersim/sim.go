// Package payersim is a simulated payer: it accepts PAS request Bundles,
// returns PAS ClaimResponses and answers status inquiries. Decision
// behaviour and latency are configurable so the engine's pend/approve/deny
// and appeal paths can be exercised end-to-end with no real payer.
//
// It is both an HTTP server (cmd/payer-sim) and an in-process
// ports.PayerGateway for `make run` and tests.
package payersim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/pas"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
)

// Mode selects the decision policy.
type Mode string

const (
	// ModeSmart approves well-documented requests, pends thin ones, and
	// flips a pended request to approved after PendFlips inquiries.
	ModeSmart Mode = "smart"
	// ModeApprove approves everything immediately.
	ModeApprove Mode = "approve"
	// ModeDeny denies everything with DenyReason.
	ModeDeny Mode = "deny"
	// ModePend pends everything; inquiries flip to approved after PendFlips.
	ModePend Mode = "pend"
	// ModeRandom picks approve/deny/pend with fixed weights (seeded).
	ModeRandom Mode = "random"
)

// Config tunes the simulator.
type Config struct {
	Mode Mode
	// Latency is added to every call (HTTP handler and in-process).
	Latency time.Duration
	// PendFlips is how many inquiries a pended request needs before it is
	// approved (smart/pend modes). 0 means "stay pended forever".
	PendFlips int
	// SmartMinEvidence is the supportingInfo count needed for an immediate
	// approval in smart mode.
	SmartMinEvidence int
	DenyReason       string
	// ErrorRate injects HTTP 503s (0..1) to exercise retry paths.
	ErrorRate float64
	Seed      uint64
}

// DefaultConfig is what `make run` uses.
func DefaultConfig() Config {
	return Config{Mode: ModeSmart, PendFlips: 1, SmartMinEvidence: 8, DenyReason: "Clinical criteria not met: insufficient documentation of conservative therapy"}
}

// Validate checks the configuration.
func (c Config) Validate() error {
	switch c.Mode {
	case ModeSmart, ModeApprove, ModeDeny, ModePend, ModeRandom:
	default:
		return fmt.Errorf("payersim: unknown mode %q", c.Mode)
	}
	if c.ErrorRate < 0 || c.ErrorRate > 1 {
		return errors.New("payersim: error_rate must be within 0..1")
	}
	return nil
}

type record struct {
	claimID    string
	patientRef string
	outcome    workflow.PayerOutcome
	inquiries  int
	authNumber string
	reasons    []string
}

// Engine holds simulator state.
type Engine struct {
	cfg   Config
	clock func() time.Time
	mu    sync.Mutex
	rng   *rand.Rand
	recs  map[string]*record
	seq   int
	stats struct{ approved, denied, pended, errors int }
}

// New creates an engine.
func New(cfg Config, clock func() time.Time) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = time.Now
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = 42
	}
	return &Engine{cfg: cfg, clock: clock, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), recs: map[string]*record{}}, nil
}

// SetMode changes the decision policy at runtime (admin endpoint).
func (e *Engine) SetMode(m Mode) error {
	c := e.cfg
	c.Mode = m
	if err := c.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	e.cfg.Mode = m
	e.mu.Unlock()
	return nil
}

// Stats returns counters for the admin endpoint.
func (e *Engine) Stats() map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return map[string]int{"approved": e.stats.approved, "denied": e.stats.denied, "pended": e.stats.pended, "errors": e.stats.errors, "records": len(e.recs)}
}

// claimSummary is what the simulator inspects in a request bundle.
type claimSummary struct {
	claimID      string
	patientRef   string
	evidence     int
	diagnoses    int
	forceOutcome string
}

func summarize(bundle []byte) (claimSummary, error) {
	var b struct {
		ResourceType string `json:"resourceType"`
		Entry        []struct {
			Resource struct {
				ResourceType   string            `json:"resourceType"`
				ID             string            `json:"id"`
				Patient        map[string]string `json:"patient"`
				SupportingInfo []json.RawMessage `json:"supportingInfo"`
				Diagnosis      []json.RawMessage `json:"diagnosis"`
				Extension      []struct {
					URL         string `json:"url"`
					ValueString string `json:"valueString"`
				} `json:"extension"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil {
		return claimSummary{}, fmt.Errorf("invalid bundle: %w", err)
	}
	if b.ResourceType != "Bundle" {
		return claimSummary{}, fmt.Errorf("expected Bundle, got %q", b.ResourceType)
	}
	for _, en := range b.Entry {
		if en.Resource.ResourceType != "Claim" {
			continue
		}
		s := claimSummary{claimID: en.Resource.ID, patientRef: en.Resource.Patient["reference"], evidence: len(en.Resource.SupportingInfo), diagnoses: len(en.Resource.Diagnosis)}
		for _, ext := range en.Resource.Extension {
			if ext.URL == "urn:priorauth:sim-decision" {
				s.forceOutcome = ext.ValueString
			}
		}
		if s.claimID == "" {
			return s, errors.New("Claim has no id")
		}
		return s, nil
	}
	return claimSummary{}, errors.New("bundle contains no Claim")
}

// Submit decides on a bundle and returns the ClaimResponse JSON.
// forced, when non-empty (from an X-Sim-Decision header), overrides Mode.
func (e *Engine) Submit(bundle []byte, forced string) ([]byte, error) {
	s, err := summarize(bundle)
	if err != nil {
		return nil, err
	}
	if forced == "" {
		forced = s.forceOutcome
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	ref := fmt.Sprintf("SIM-%06d", e.seq)
	rec := &record{claimID: s.claimID, patientRef: s.patientRef}
	switch strings.ToLower(forced) {
	case "approve", "approved":
		rec.outcome = workflow.OutcomeApproved
	case "deny", "denied":
		rec.outcome = workflow.OutcomeDenied
	case "pend", "pended":
		rec.outcome = workflow.OutcomePended
	default:
		rec.outcome = e.decide(s)
	}
	e.finish(rec, ref)
	e.recs[ref] = rec
	return e.response(ref, rec), nil
}

func (e *Engine) decide(s claimSummary) workflow.PayerOutcome {
	switch e.cfg.Mode {
	case ModeApprove:
		return workflow.OutcomeApproved
	case ModeDeny:
		return workflow.OutcomeDenied
	case ModePend:
		return workflow.OutcomePended
	case ModeRandom:
		switch x := e.rng.Float64(); {
		case x < 0.6:
			return workflow.OutcomeApproved
		case x < 0.8:
			return workflow.OutcomePended
		default:
			return workflow.OutcomeDenied
		}
	}
	// smart
	if s.diagnoses == 0 {
		return workflow.OutcomeDenied
	}
	if s.evidence >= e.cfg.SmartMinEvidence {
		return workflow.OutcomeApproved
	}
	return workflow.OutcomePended
}

func (e *Engine) finish(rec *record, ref string) {
	switch rec.outcome {
	case workflow.OutcomeApproved:
		rec.authNumber = "AUTH-" + strings.TrimPrefix(ref, "SIM-")
		rec.reasons = nil
		e.stats.approved++
	case workflow.OutcomeDenied:
		rec.reasons = []string{e.cfg.DenyReason}
		e.stats.denied++
	default:
		rec.reasons = []string{"Pended for medical review"}
		e.stats.pended++
	}
}

// Inquire returns the current ClaimResponse for a payer reference,
// flipping pended records to approved after PendFlips inquiries.
func (e *Engine) Inquire(ref string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, ok := e.recs[ref]
	if !ok {
		return nil, ErrUnknownRef
	}
	if rec.outcome == workflow.OutcomePended {
		rec.inquiries++
		if e.cfg.PendFlips > 0 && rec.inquiries >= e.cfg.PendFlips {
			rec.outcome = workflow.OutcomeApproved
			e.stats.pended--
			e.finish(rec, ref)
		}
	}
	return e.response(ref, rec), nil
}

// ErrUnknownRef is returned for inquiries about unknown references.
var ErrUnknownRef = errors.New("payersim: unknown payer reference")

func (e *Engine) response(ref string, rec *record) []byte {
	return pas.BuildResponse(ref, rec.claimID, rec.patientRef, rec.outcome, rec.authNumber, rec.reasons, e.clock())
}

func (e *Engine) shouldFail() bool {
	if e.cfg.ErrorRate <= 0 {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rng.Float64() < e.cfg.ErrorRate {
		e.stats.errors++
		return true
	}
	return false
}

func (e *Engine) sleep(ctx context.Context) error {
	if e.cfg.Latency <= 0 {
		return nil
	}
	t := time.NewTimer(e.cfg.Latency)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---------------------------------------------------------------------------
// In-process gateway.

// Gateway adapts the engine to ports.PayerGateway without HTTP.
type Gateway struct{ E *Engine }

// Submit implements ports.PayerGateway.
func (g Gateway) Submit(ctx context.Context, bundle []byte) ([]byte, error) {
	if err := g.E.sleep(ctx); err != nil {
		return nil, err
	}
	if g.E.shouldFail() {
		return nil, errors.New("payersim: injected transport failure")
	}
	return g.E.Submit(bundle, "")
}

// Inquire implements ports.PayerGateway.
func (g Gateway) Inquire(ctx context.Context, ref string) ([]byte, error) {
	if err := g.E.sleep(ctx); err != nil {
		return nil, err
	}
	return g.E.Inquire(ref)
}

// Ping implements ports.PayerGateway.
func (g Gateway) Ping(context.Context) error { return nil }

// ---------------------------------------------------------------------------
// HTTP server.

// Handler exposes the FHIR-ish endpoints:
//
//	POST /Claim/$submit          body: PAS request Bundle → ClaimResponse
//	GET  /ClaimResponse/{ref}    → current ClaimResponse
//	GET  /healthz
//	GET  /admin/stats
//	PUT  /admin/mode             body: {"mode":"approve"}
func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /Claim/$submit", func(w http.ResponseWriter, r *http.Request) {
		if err := e.sleep(r.Context()); err != nil {
			return
		}
		if e.shouldFail() {
			http.Error(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"transient","diagnostics":"injected failure"}]}`, http.StatusServiceUnavailable)
			return
		}
		body, err := readBody(r, 4<<20)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := e.Submit(body, r.Header.Get("X-Sim-Decision"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeFHIR(w, http.StatusOK, resp)
	})
	mux.HandleFunc("GET /ClaimResponse/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if err := e.sleep(r.Context()); err != nil {
			return
		}
		resp, err := e.Inquire(r.PathValue("ref"))
		if errors.Is(err, ErrUnknownRef) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeFHIR(w, http.StatusOK, resp)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /admin/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(e.Stats())
	})
	mux.HandleFunc("PUT /admin/mode", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Mode Mode `json:"mode"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := e.SetMode(body.Mode); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("body exceeds %d bytes", limit)
	}
	if len(body) == 0 {
		return nil, errors.New("empty body")
	}
	return body, nil
}

func writeFHIR(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/fhir+json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
