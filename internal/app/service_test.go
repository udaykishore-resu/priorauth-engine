package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/llm"
	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/memory"
	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/payersim"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/rules"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
	"github.com/udaykishore-resu/priorauth-engine/internal/ports"
)

const examplesDir = "../../examples"

// fakeClock is a settable clock shared by the service under test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type harness struct {
	svc   *Service
	repo  *memory.Repository
	pub   *memory.Publisher
	sim   *payersim.Engine
	clock *fakeClock
	store *rules.Store
}

func newHarness(t *testing.T, simMode payersim.Mode, proposers ...evidence.Extractor) *harness {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	set, err := rules.DirLoader{Dir: "../../rules", Now: clock.Now}.Load()
	require.NoError(t, err)
	store := rules.NewStore(set)
	simCfg := payersim.DefaultConfig()
	simCfg.Mode = simMode
	sim, err := payersim.New(simCfg, clock.Now)
	require.NoError(t, err)
	repo := memory.NewRepository()
	pub := memory.NewPublisher()
	n := 0
	svc, err := New(Deps{
		Repo: repo, Publisher: pub, Rules: store, Gateway: payersim.Gateway{E: sim},
		Structured: evidence.FHIRExtractor{}, Proposers: proposers,
		SLA:    workflow.SLAPolicy{Determine: time.Hour, Assemble: 2 * time.Hour, Submit: 2 * time.Hour, Response: time.Hour, Pended: 24 * time.Hour, Review: 4 * time.Hour, UrgentFactor: 0.5},
		Clock:  clock.Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewID:  func() string { n++; return fmt.Sprintf("id-%04d", n) },
	})
	require.NoError(t, err)
	return &harness{svc: svc, repo: repo, pub: pub, sim: sim, clock: clock, store: store}
}

func loadExample(t *testing.T, name string) workflow.Intake {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(examplesDir, name))
	require.NoError(t, err)
	var in workflow.Intake
	require.NoError(t, json.Unmarshal(b, &in))
	return in
}

func TestNew_RequiresDeps(t *testing.T) {
	_, err := New(Deps{})
	require.Error(t, err)
}

func TestExample01_MRI_HappyPath(t *testing.T) {
	h := newHarness(t, payersim.ModeSmart)
	ctx := context.Background()

	r, created, err := h.svc.Create(ctx, loadExample(t, "01-mri-lumbar-spine.json"))
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, workflow.StateDraft, r.State)

	// Idempotent replay returns the same request.
	again, created, err := h.svc.Create(ctx, loadExample(t, "01-mri-lumbar-spine.json"))
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, r.ID, again.ID)

	r, err = h.svc.Determine(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.StateDetermined, r.State)
	require.Equal(t, rules.Required, r.Determination.Decision)
	require.Equal(t, rules.Met, r.Determination.Criteria)
	require.Equal(t, "acme.imaging.mri-lumbar-spine@3", r.Determination.RuleRef)
	require.NotEmpty(t, r.Determination.RuleSetHash)
	require.Empty(t, r.Exceptions)

	r, err = h.svc.Assemble(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.StateAssembled, r.State)
	require.True(t, r.Evidence.Complete, "%+v", r.Evidence.Documentation)
	require.Equal(t, 0, r.Evidence.Proposals, "no LLM configured → no proposals")
	require.Equal(t, []string{evidence.FHIRExtractorName}, r.Evidence.Extractors)

	r, err = h.svc.Submit(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.StateApproved, r.State, "well documented → smart sim approves immediately")
	require.NotEmpty(t, r.Decision.AuthNumber)
	require.Equal(t, 1, r.Submission.Attempt)

	// Event stream is complete and ordered; the projection matches a rebuild.
	evs, err := h.svc.Events(ctx, r.ID)
	require.NoError(t, err)
	types := make([]string, 0, len(evs))
	for i, e := range evs {
		require.Equal(t, i+1, e.Seq)
		require.NotEmpty(t, e.ID)
		types = append(types, e.Type)
	}
	require.Equal(t, []string{
		workflow.TypeRequestCreated, workflow.TypeDeterminationRecorded, workflow.TypeEvidenceAssembled,
		workflow.TypeSubmitted, workflow.TypePayerResponded,
	}, types)
	rebuilt, err := h.repo.Load(ctx, r.ID)
	require.NoError(t, err)
	view, err := h.svc.Get(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, rebuilt.State, view.State)
	require.Equal(t, rebuilt.Version, view.Version)
	require.Len(t, h.pub.Sent(), len(evs), "every committed event was published")

	// Terminal: further commands are rejected with a conflict.
	_, err = h.svc.Submit(ctx, r.ID)
	require.ErrorIs(t, err, workflow.ErrInvalidTransition)
	_, err = h.svc.Determine(ctx, r.ID)
	require.ErrorIs(t, err, workflow.ErrInvalidTransition)

	st, err := h.svc.Turnaround(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, st.Total)
	require.Equal(t, 1, st.Decided.Count)
	require.Equal(t, 1, st.Payer["ACME_HEALTH"]["approved"])
	require.Equal(t, 1.0, st.AutoDetermined)
}

func TestExample02_GLP1_Determination(t *testing.T) {
	h := newHarness(t, payersim.ModeApprove)
	ctx := context.Background()
	r, _, err := h.svc.Create(ctx, loadExample(t, "02-glp1-semaglutide.json"))
	require.NoError(t, err)
	r, err = h.svc.Determine(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, rules.Required, r.Determination.Decision)
	require.Equal(t, rules.Met, r.Determination.Criteria, "missing: %v", r.Determination.MissingData)
	require.Equal(t, "acme.pharmacy.glp1-semaglutide@5", r.Determination.RuleRef)
	r, err = h.svc.Assemble(ctx, r.ID)
	require.NoError(t, err)
	require.True(t, r.Evidence.Complete)
	r, err = h.svc.Submit(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.StateApproved, r.State)
}

func TestExample03_PT_PendedThenApprovedViaPoll(t *testing.T) {
	h := newHarness(t, payersim.ModeSmart)
	ctx := context.Background()
	r, _, err := h.svc.Create(ctx, loadExample(t, "03-physical-therapy.json"))
	require.NoError(t, err)
	r, err = h.svc.Determine(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, rules.Met, r.Determination.Criteria, "missing: %v", r.Determination.MissingData)
	r, err = h.svc.Assemble(ctx, r.ID)
	require.NoError(t, err)
	r, err = h.svc.Submit(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.StatePended, r.State, "thin evidence → pended")
	require.NotEmpty(t, r.Submission.PayerRef)

	// The pended SLA has not elapsed; nothing breaches.
	n, err := h.svc.CheckSLAs(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n)

	// Poll flips it to approved (sim PendFlips=1).
	n, err = h.svc.PollPended(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	r, err = h.svc.Get(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.StateApproved, r.State)

	// Sync on a terminal request is a conflict.
	_, err = h.svc.Sync(ctx, r.ID)
	require.ErrorIs(t, err, workflow.ErrInvalidTransition)
}

func TestPlanWaiver_NotRequired(t *testing.T) {
	h := newHarness(t, payersim.ModeSmart)
	ctx := context.Background()
	in := loadExample(t, "03-physical-therapy.json")
	in.IdempotencyKey = "waiver"
	in.Payer.Plan = "PPO-PLATINUM"
	r, _, err := h.svc.Create(ctx, in)
	require.NoError(t, err)
	r, err = h.svc.Determine(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, rules.NotRequired, r.Determination.Decision)
	require.False(t, r.DecidedAt.IsZero())
	_, err = h.svc.Assemble(ctx, r.ID)
	require.ErrorIs(t, err, workflow.ErrInvalidTransition)
}

func TestExample04_ReviewOverride_Pend_Deny_Appeal(t *testing.T) {
	h := newHarness(t, payersim.ModeSmart)
	ctx := context.Background()
	r, _, err := h.svc.Create(ctx, loadExample(t, "04-mri-needs-review.json"))
	require.NoError(t, err)
	r, err = h.svc.Determine(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, rules.Required, r.Determination.Decision)
	require.Equal(t, rules.Unknown, r.Determination.Criteria)
	require.NotEmpty(t, r.Determination.MissingData)

	r, err = h.svc.Assemble(ctx, r.ID)
	require.NoError(t, err)
	require.False(t, r.Evidence.Complete)
	require.Len(t, r.Exceptions, 1)
	require.Equal(t, workflow.ExcCriteriaUnk, r.Exceptions[0].Code)
	_, err = h.svc.Submit(ctx, r.ID)
	require.ErrorIs(t, err, workflow.ErrInvalidTransition)

	// Exceptions queue shows it, urgent first, with a suggested action.
	q, err := h.svc.Exceptions(ctx)
	require.NoError(t, err)
	require.Len(t, q, 1)
	require.Equal(t, r.ID, q[0].RequestID)
	require.True(t, q[0].Urgent)
	require.Contains(t, q[0].SuggestedAction, "override_criteria")

	// Clinician override, then re-assemble → complete.
	var rv workflow.Review
	b, err := os.ReadFile(filepath.Join(examplesDir, "review-override.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &rv))
	r, err = h.svc.Review(ctx, r.ID, rv)
	require.NoError(t, err)
	require.Empty(t, r.Exceptions)
	r, err = h.svc.Assemble(ctx, r.ID)
	require.NoError(t, err)
	require.True(t, r.Evidence.Complete)

	// Force a denial via the simulator, then appeal.
	require.NoError(t, h.sim.SetMode(payersim.ModeDeny))
	r, err = h.svc.Submit(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.StateDenied, r.State)
	require.Len(t, r.Exceptions, 1)
	require.Equal(t, workflow.ExcDenied, r.Exceptions[0].Code)

	b, err = os.ReadFile(filepath.Join(examplesDir, "review-appeal.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &rv))
	r, err = h.svc.Review(ctx, r.ID, rv)
	require.NoError(t, err)
	require.Equal(t, workflow.StateAppealed, r.State)
	require.Empty(t, r.Exceptions)

	require.NoError(t, h.sim.SetMode(payersim.ModePend))
	r, err = h.svc.Submit(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.StateAppealed, r.State, "pended appeal stays appealed")
	require.True(t, r.Submission.Appeal)
	require.Equal(t, 2, r.Submission.Attempt)

	// Urgent request: response SLA (1h * 0.5) breaches after 31 minutes.
	h.clock.Advance(31 * time.Minute)
	n, err := h.svc.CheckSLAs(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	r, err = h.svc.Get(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, []workflow.Timer{workflow.TimerResponse}, r.Breached)
	n, err = h.svc.CheckSLAs(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "a breached timer fires once")

	// The appeal outcome arrives via poll.
	n, err = h.svc.PollPended(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	r, err = h.svc.Get(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.StateApproved, r.State)
	require.Empty(t, r.Exceptions, "approval clears SLA exceptions")

	st, err := h.svc.Turnaround(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, st.SLABreaches[workflow.TimerResponse])
	require.Equal(t, 1, st.Payer["ACME_HEALTH"]["approved"])
}

// fakeLLM is an OpenAI-compatible endpoint that returns canned facts.
func fakeLLM(t *testing.T, content string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		require.Contains(t, string(body), "NOTE TYPE: clinical_note")
		require.NotContains(t, string(body), "resourceType", "structured data never reaches the model")
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		resp := map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": content}}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLLMProposals_RequireConfirmation(t *testing.T) {
	content := `{"facts":[
	  {"kind":"procedure","code":"97110","display":"PT sessions","snippet":"Completed 8 physical therapy sessions (97110)","confidence":0.9,"effective":"2026-08-15"},
	  {"kind":"observation","code":"PA-NEURO-DEFICIT","display":"foot drop","snippet":"New foot drop on the right","confidence":0.85},
	  {"kind":"diagnosis","code":"C79.51","display":"HALLUCINATED","snippet":"metastatic disease","confidence":0.9},
	  {"kind":"diagnosis","code":"M54.50","display":"LBP","snippet":"acute low back pain","confidence":0.3}
	]}`
	srv := fakeLLM(t, content, http.StatusOK)
	ext := llm.New(srv.URL, "test-key", "test-model", 5*time.Second)
	h := newHarness(t, payersim.ModeApprove, ext)
	ctx := context.Background()

	r, _, err := h.svc.Create(ctx, loadExample(t, "04-mri-needs-review.json"))
	require.NoError(t, err)
	_, err = h.svc.Determine(ctx, r.ID)
	require.NoError(t, err)
	r, err = h.svc.Assemble(ctx, r.ID)
	require.NoError(t, err)

	// Two proposals survive: the hallucinated one (snippet absent) and the
	// low-confidence one are dropped. Criteria stay unknown because
	// proposals are not usable yet.
	require.Equal(t, 2, r.Evidence.Proposals)
	require.Equal(t, rules.Unknown, r.Evidence.Criteria)
	require.False(t, r.Evidence.Complete)
	codes := map[string]bool{}
	for _, x := range r.Exceptions {
		codes[x.Code] = true
	}
	require.True(t, codes[workflow.ExcEvidenceReview])
	require.True(t, codes[workflow.ExcCriteriaUnk])
	require.Equal(t, []string{evidence.FHIRExtractorName, ext.Name()}, r.Evidence.Extractors)

	var ids []string
	for _, f := range r.Evidence.Facts {
		if f.Prov.Origin == evidence.OriginLLM {
			require.False(t, f.Usable())
			require.NotEmpty(t, f.Prov.Snippet)
			ids = append(ids, f.ID)
		}
	}
	require.Len(t, ids, 2)

	// A reviewer confirms the neuro-deficit observation and rejects the PT
	// count; re-assembly now meets the red-flag branch.
	var neuro, pt string
	for _, f := range r.Evidence.Facts {
		switch f.Code {
		case "PA-NEURO-DEFICIT":
			neuro = f.ID
		case "97110":
			pt = f.ID
		}
	}
	r, err = h.svc.Review(ctx, r.ID, workflow.Review{Action: workflow.ActionRejectEvidence, Actor: "md", FactIDs: []string{pt}})
	require.NoError(t, err)
	require.Contains(t, exceptionCodes(r), workflow.ExcEvidenceReview, "one proposal still pending")
	r, err = h.svc.Review(ctx, r.ID, workflow.Review{Action: workflow.ActionConfirmEvidence, Actor: "md", FactIDs: []string{neuro}})
	require.NoError(t, err)
	require.NotContains(t, exceptionCodes(r), workflow.ExcEvidenceReview)

	r, err = h.svc.Assemble(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, 0, r.Evidence.Proposals)
	require.Equal(t, rules.Met, r.Evidence.Criteria, "missing: %v", r.Evidence.MissingData)
	require.True(t, r.Evidence.Complete)
	require.Empty(t, r.Exceptions)
	for _, f := range r.Evidence.Facts {
		require.NotEqual(t, pt, f.ID, "rejected proposal removed from the package")
		if f.ID == neuro {
			require.Equal(t, "md", f.Prov.ConfirmedBy)
		}
	}

	r, err = h.svc.Submit(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.StateApproved, r.State)
}

func TestLLMFailure_DegradesWithWarning(t *testing.T) {
	srv := fakeLLM(t, "", http.StatusBadGateway)
	ext := llm.New(srv.URL, "test-key", "m", time.Second)
	h := newHarness(t, payersim.ModeApprove, ext)
	ctx := context.Background()
	r, _, err := h.svc.Create(ctx, loadExample(t, "04-mri-needs-review.json"))
	require.NoError(t, err)
	_, err = h.svc.Determine(ctx, r.ID)
	require.NoError(t, err)
	r, err = h.svc.Assemble(ctx, r.ID)
	require.NoError(t, err)
	require.Len(t, r.Evidence.Warnings, 1)
	require.Contains(t, r.Evidence.Warnings[0], "status 502")
	require.Equal(t, 0, r.Evidence.Proposals)
}

func TestIndeterminate_SetDeterminationByHuman(t *testing.T) {
	h := newHarness(t, payersim.ModeApprove)
	ctx := context.Background()
	in := loadExample(t, "01-mri-lumbar-spine.json")
	in.Payer.ID = "UNKNOWN_PAYER"
	r, _, err := h.svc.Create(ctx, in)
	require.NoError(t, err)
	r, err = h.svc.Determine(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, rules.Indeterminate, r.Determination.Decision)
	require.Equal(t, workflow.ExcIndeterminate, r.Exceptions[0].Code)
	r, err = h.svc.Review(ctx, r.ID, workflow.Review{Action: workflow.ActionSetDetermination, Actor: "um", Decision: rules.NotRequired})
	require.NoError(t, err)
	require.Empty(t, r.Exceptions)
	require.Equal(t, rules.NotRequired, r.Determination.Decision)
	st, err := h.svc.Turnaround(ctx)
	require.NoError(t, err)
	require.Equal(t, 0.0, st.AutoDetermined)
	require.Equal(t, 1, st.ByOutcome["not_required"].Count)
}

type failingGateway struct{ err error }

func (f failingGateway) Submit(context.Context, []byte) ([]byte, error)  { return nil, f.err }
func (f failingGateway) Inquire(context.Context, string) ([]byte, error) { return nil, f.err }
func (f failingGateway) Ping(context.Context) error                      { return f.err }

type badResponseGateway struct{}

func (badResponseGateway) Submit(context.Context, []byte) ([]byte, error) {
	return []byte(`{"resourceType":"OperationOutcome"}`), nil
}
func (badResponseGateway) Inquire(context.Context, string) ([]byte, error) {
	return []byte(`not json`), nil
}
func (badResponseGateway) Ping(context.Context) error { return nil }

func assembled(t *testing.T, h *harness) *workflow.Request {
	t.Helper()
	ctx := context.Background()
	r, _, err := h.svc.Create(ctx, loadExample(t, "01-mri-lumbar-spine.json"))
	require.NoError(t, err)
	_, err = h.svc.Determine(ctx, r.ID)
	require.NoError(t, err)
	r, err = h.svc.Assemble(ctx, r.ID)
	require.NoError(t, err)
	return r
}

func TestSubmit_GatewayFailureIsRetryable(t *testing.T) {
	h := newHarness(t, payersim.ModeApprove)
	h.svc.d.Gateway = failingGateway{err: errors.New("connection refused")}
	ctx := context.Background()
	r := assembled(t, h)
	r, err := h.svc.Submit(ctx, r.ID)
	require.ErrorIs(t, err, ErrPayerUnavailable)
	require.NotNil(t, r)
	require.Equal(t, workflow.StateAssembled, r.State)
	require.Equal(t, workflow.ExcSubmitFailed, r.Exceptions[0].Code)

	// The gateway recovers; retry succeeds and clears the exception.
	h.svc.d.Gateway = payersim.Gateway{E: h.sim}
	r, err = h.svc.Submit(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.StateApproved, r.State)
	require.Empty(t, r.Exceptions)
}

func TestSubmit_UninterpretableResponse(t *testing.T) {
	h := newHarness(t, payersim.ModeApprove)
	h.svc.d.Gateway = badResponseGateway{}
	ctx := context.Background()
	r := assembled(t, h)
	r, err := h.svc.Submit(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.StateSubmitted, r.State)
	require.Equal(t, workflow.ExcPayerInvalid, r.Exceptions[0].Code)

	// Sync with a bad body keeps the exception; a good sim answer resolves it.
	r.Submission.PayerRef = "x"
	_, err = h.svc.Sync(ctx, r.ID)
	require.ErrorIs(t, err, workflow.ErrInvalidTransition, "no payer ref recorded → nothing to inquire")

	q, err := h.svc.Exceptions(ctx)
	require.NoError(t, err)
	require.Len(t, q, 1)
	require.Contains(t, q[0].SuggestedAction, "raw payer response")
}

func TestSLA_DraftAndReviewTimers(t *testing.T) {
	h := newHarness(t, payersim.ModeApprove)
	ctx := context.Background()
	r, _, err := h.svc.Create(ctx, loadExample(t, "03-physical-therapy.json"))
	require.NoError(t, err)

	h.clock.Advance(61 * time.Minute)
	n, err := h.svc.CheckSLAs(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	r, err = h.svc.Get(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, "sla_determine", r.Exceptions[0].Code)
	require.Equal(t, workflow.StateDraft, r.State, "breach does not change state")

	q, err := h.svc.Exceptions(ctx)
	require.NoError(t, err)
	require.Equal(t, "0s", q[0].Age, "raised at check time")

	// Review clock (4h) runs from the oldest open exception.
	h.clock.Advance(4 * time.Hour)
	n, err = h.svc.CheckSLAs(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	r, err = h.svc.Get(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, []workflow.Timer{workflow.TimerDetermine, workflow.TimerReview}, r.Breached)

	// Withdrawal closes everything.
	r, err = h.svc.Review(ctx, r.ID, workflow.Review{Action: workflow.ActionWithdraw, Actor: "clerk", Reason: "duplicate"})
	require.NoError(t, err)
	require.Equal(t, workflow.StateClosed, r.State)
	require.Empty(t, r.Exceptions)
	list, err := h.svc.List(ctx, ports.Filter{Active: true})
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestCreate_ValidationAndContentKey(t *testing.T) {
	h := newHarness(t, payersim.ModeApprove)
	ctx := context.Background()
	_, _, err := h.svc.Create(ctx, workflow.Intake{})
	require.ErrorIs(t, err, workflow.ErrValidation)

	in := loadExample(t, "03-physical-therapy.json")
	in.IdempotencyKey = ""
	a, created, err := h.svc.Create(ctx, in)
	require.NoError(t, err)
	require.True(t, created)
	require.True(t, strings.HasPrefix(a.Intake.IdempotencyKey, "sha256:"))
	b, created, err := h.svc.Create(ctx, in)
	require.NoError(t, err)
	require.False(t, created, "same content → same request")
	require.Equal(t, a.ID, b.ID)

	in.Service.Units = 6
	c, created, err := h.svc.Create(ctx, in)
	require.NoError(t, err)
	require.True(t, created, "different content → new request")
	require.NotEqual(t, a.ID, c.ID)

	_, err = h.svc.Get(ctx, "nope")
	require.ErrorIs(t, err, workflow.ErrNotFound)
	_, err = h.svc.Determine(ctx, "nope")
	require.ErrorIs(t, err, workflow.ErrNotFound)
}

func TestRulesHotReloadAffectsNextDetermination(t *testing.T) {
	h := newHarness(t, payersim.ModeApprove)
	ctx := context.Background()
	in := loadExample(t, "03-physical-therapy.json")
	r, _, err := h.svc.Create(ctx, in)
	require.NoError(t, err)

	waiver, err := rules.NewSet([]rules.Rule{{ID: "all-waived", Version: 1, Payer: "*", ServiceCodes: []string{"*"}}}, h.clock.Now())
	require.NoError(t, err)
	h.store.Replace(waiver)
	r, err = h.svc.Determine(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, rules.NotRequired, r.Determination.Decision)
	require.Equal(t, waiver.Hash, r.Determination.RuleSetHash, "decision records the rule set that produced it")
	require.Equal(t, int64(1), h.store.Reloads())
}

func TestPercentiles(t *testing.T) {
	require.Equal(t, Percentiles{}, percentiles(nil))
	p := percentiles([]float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100})
	require.Equal(t, 10, p.Count)
	require.InDelta(t, 60, p.P50, 10)
	require.InDelta(t, 100, p.P95, 10)
	require.Equal(t, 55.0, p.Mean)
	require.Equal(t, 100.0, p.Max)
}

func exceptionCodes(r *workflow.Request) []string {
	var out []string
	for _, x := range r.Exceptions {
		out = append(out, x.Code)
	}
	return out
}
