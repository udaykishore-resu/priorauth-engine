// Package app is the application layer: it orchestrates the pure domain
// (rules, evidence, workflow, pas) with the ports (repository, publisher,
// payer gateway) and records metrics. Every public method is one use case
// and one HTTP endpoint maps onto one method.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/pas"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/rules"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
	"github.com/udaykishore-resu/priorauth-engine/internal/observability"
	"github.com/udaykishore-resu/priorauth-engine/internal/ports"
)

// ErrPayerUnavailable wraps transport failures towards the payer.
var ErrPayerUnavailable = errors.New("payer unavailable")

// ActorEngine is the actor recorded on machine-generated events.
const ActorEngine = "engine"

// Deps are the collaborators the service needs.
type Deps struct {
	Repo       ports.Repository
	Publisher  ports.Publisher
	Rules      *rules.Store
	Gateway    ports.PayerGateway
	Structured evidence.Extractor   // required; deterministic
	Proposers  []evidence.Extractor // optional; e.g. the LLM extractor
	SLA        workflow.SLAPolicy
	Clock      ports.Clock
	Logger     *slog.Logger
	Metrics    *observability.Metrics
	// NewID overrides id generation (tests).
	NewID func() string
}

// Service implements the use cases.
type Service struct {
	d      Deps
	tracer trace.Tracer
}

// New validates deps and returns a Service.
func New(d Deps) (*Service, error) {
	if d.Repo == nil || d.Rules == nil || d.Gateway == nil || d.Structured == nil {
		return nil, errors.New("app: Repo, Rules, Gateway and Structured are required")
	}
	if d.Publisher == nil {
		d.Publisher = noopPublisher{}
	}
	if d.Clock == nil {
		d.Clock = func() time.Time { return time.Now().UTC() }
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Metrics == nil {
		d.Metrics = observability.NewMetrics()
	}
	if d.NewID == nil {
		d.NewID = uuid.NewString
	}
	s := &Service{d: d, tracer: observability.Tracer()}
	d.Metrics.RulesLoaded.Set(float64(len(d.Rules.Current().Rules)))
	d.Rules.OnSwap(func(_, next *rules.Set) {
		d.Metrics.RulesLoaded.Set(float64(len(next.Rules)))
		d.Metrics.RuleReloads.Inc()
	})
	return s, nil
}

type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, []workflow.Envelope) error { return nil }
func (noopPublisher) Close() error                                       { return nil }

// Rules exposes the active rule set (GET /v1/rules).
func (s *Service) Rules() *rules.Set { return s.d.Rules.Current() }

// Get returns the projected request.
func (s *Service) Get(ctx context.Context, id string) (*workflow.Request, error) {
	return s.d.Repo.Get(ctx, id)
}

// Events returns the raw event stream (audit).
func (s *Service) Events(ctx context.Context, id string) ([]workflow.Envelope, error) {
	return s.d.Repo.Events(ctx, id)
}

// List returns projected requests.
func (s *Service) List(ctx context.Context, f ports.Filter) ([]*workflow.Request, error) {
	return s.d.Repo.List(ctx, f)
}

// Create opens a request. It is idempotent on Intake.IdempotencyKey (or a
// content hash when the key is absent): a replay returns the original
// request and created=false.
func (s *Service) Create(ctx context.Context, in workflow.Intake) (*workflow.Request, bool, error) {
	ctx, span := s.tracer.Start(ctx, "app.Create")
	defer span.End()
	if err := in.Validate(); err != nil {
		return nil, false, err
	}
	key := in.IdempotencyKey
	if key == "" {
		key = contentKey(in)
		in.IdempotencyKey = key
	}
	id := s.d.NewID()
	existing, created, err := s.d.Repo.Reserve(ctx, key, id)
	if err != nil {
		return nil, false, fmt.Errorf("app: reserve idempotency key: %w", err)
	}
	if !created {
		r, err := s.d.Repo.Get(ctx, existing)
		if err != nil {
			return nil, false, fmt.Errorf("app: idempotent replay of %s: %w", existing, err)
		}
		span.SetAttributes(attribute.Bool("idempotent_replay", true))
		return r, false, nil
	}
	now := s.d.Clock()
	r, evs, err := workflow.Create(id, in, now)
	if err != nil {
		return nil, false, err
	}
	if err := s.commit(ctx, r, evs, ActorEngine, now); err != nil {
		return nil, false, err
	}
	span.SetAttributes(attribute.String("request.id", r.ID))
	return r, true, nil
}

// Determine runs the deterministic rule evaluation over structured facts.
func (s *Service) Determine(ctx context.Context, id string) (*workflow.Request, error) {
	ctx, span := s.tracer.Start(ctx, "app.Determine", trace.WithAttributes(attribute.String("request.id", id)))
	defer span.End()
	r, err := s.d.Repo.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.d.Clock()
	facts, err := s.d.Structured.Extract(ctx, s.input(r, now, false))
	if err != nil {
		return nil, fmt.Errorf("%w: structured extraction: %w", workflow.ErrValidation, err)
	}
	set := s.d.Rules.Current()
	det := set.Determine(r.RuleRequest(), facts, now)
	evs, err := r.RecordDetermination(det, now)
	if err != nil {
		return nil, err
	}
	if err := s.commit(ctx, r, evs, ActorEngine, now); err != nil {
		return nil, err
	}
	s.d.Metrics.Determinations.WithLabelValues(string(det.Decision), string(det.Criteria), det.RuleRef).Inc()
	span.SetAttributes(attribute.String("determination.decision", string(det.Decision)), attribute.String("determination.rule", det.RuleRef))
	s.d.Logger.InfoContext(ctx, "determination recorded", "request_id", r.ID, "decision", det.Decision, "criteria", det.Criteria, "rule", det.RuleRef, "rule_set", det.RuleSetHash, "facts_used", det.FactsUsed)
	return r, nil
}

// Assemble runs every extractor (structured first, then proposers such as
// the LLM adapter), corroborates proposals, re-evaluates criteria and
// records the evidence package.
func (s *Service) Assemble(ctx context.Context, id string) (*workflow.Request, error) {
	ctx, span := s.tracer.Start(ctx, "app.Assemble", trace.WithAttributes(attribute.String("request.id", id)))
	defer span.End()
	r, err := s.d.Repo.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := r.CanAssemble(); err != nil {
		return nil, err
	}
	now := s.d.Clock()
	facts, err := s.d.Structured.Extract(ctx, s.input(r, now, false))
	if err != nil {
		return nil, fmt.Errorf("%w: structured extraction: %w", workflow.ErrValidation, err)
	}
	names := []string{s.d.Structured.Name()}
	var warnings []string
	for _, p := range s.d.Proposers {
		names = append(names, p.Name())
		proposed, err := p.Extract(ctx, s.input(r, now, true))
		if err != nil {
			// Degrade, never fail: a down LLM must not block a determination.
			warnings = append(warnings, p.Name()+": "+err.Error())
			s.d.Logger.WarnContext(ctx, "proposer failed", "request_id", r.ID, "extractor", p.Name(), "err", err)
		}
		facts = append(facts, proposed...)
	}
	pkg := r.BuildPackage(s.d.Rules.Current(), facts, names, now)
	pkg.Warnings = warnings
	for _, f := range pkg.Facts {
		if f.Prov.Origin == evidence.OriginLLM {
			s.d.Metrics.Proposals.WithLabelValues(fmt.Sprint(f.Prov.Corroborated)).Inc()
		}
	}
	evs, err := r.RecordEvidence(pkg, now)
	if err != nil {
		return nil, err
	}
	if err := s.commit(ctx, r, evs, ActorEngine, now); err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.String("evidence.criteria", string(pkg.Criteria)), attribute.Bool("evidence.complete", pkg.Complete), attribute.Int("evidence.proposals", pkg.Proposals))
	s.d.Logger.InfoContext(ctx, "evidence assembled", "request_id", r.ID, "criteria", pkg.Criteria, "complete", pkg.Complete, "facts", len(pkg.Facts), "proposals", pkg.Proposals, "warnings", len(warnings))
	return r, nil
}

// Submit builds the PAS bundle, sends it and records the payer's answer.
func (s *Service) Submit(ctx context.Context, id string) (*workflow.Request, error) {
	ctx, span := s.tracer.Start(ctx, "app.Submit", trace.WithAttributes(attribute.String("request.id", id)))
	defer span.End()
	r, err := s.d.Repo.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := r.CanSubmit(); err != nil {
		return nil, err
	}
	now := s.d.Clock()
	claimID := s.d.NewID()
	bundle, err := pas.BuildRequest(r, claimID, now)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := s.d.Gateway.Submit(ctx, bundle)
	s.d.Metrics.PayerLatency.WithLabelValues("submit", resultLabel(err)).Observe(time.Since(start).Seconds())
	if err != nil {
		s.d.Metrics.Submissions.WithLabelValues("failed").Inc()
		evs, gerr := r.SubmissionFailed(err.Error(), now)
		if gerr == nil {
			if cerr := s.commit(ctx, r, evs, ActorEngine, now); cerr != nil {
				return nil, cerr
			}
		}
		span.SetStatus(codes.Error, err.Error())
		return r, fmt.Errorf("%w: %w", ErrPayerUnavailable, err)
	}
	decision, ierr := pas.Interpret(resp, now)
	evs, err := r.MarkSubmitted(claimID, decision.PayerRef, bundle, now)
	if err != nil {
		return nil, err
	}
	var st staged
	if err := s.stage(ctx, r, evs, ActorEngine, now, &st); err != nil {
		return nil, err
	}
	s.d.Metrics.Submissions.WithLabelValues("sent").Inc()
	if ierr != nil {
		evs = r.RecordInvalidPayerResponse(ierr.Error(), now)
		s.d.Logger.ErrorContext(ctx, "payer response not interpretable", "request_id", r.ID, "err", ierr)
	} else {
		evs, err = r.RecordPayerDecision(decision, now)
		if err != nil {
			return nil, err
		}
		s.d.Metrics.PayerDecisions.WithLabelValues(string(decision.Outcome)).Inc()
	}
	if err := s.stage(ctx, r, evs, ActorEngine, now, &st); err != nil {
		return nil, err
	}
	if err := s.flush(ctx, r, &st, now); err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.String("payer.outcome", string(decision.Outcome)))
	s.d.Logger.InfoContext(ctx, "submitted", "request_id", r.ID, "claim_id", claimID, "payer_ref", decision.PayerRef, "outcome", decision.Outcome, "attempt", r.Submission.Attempt)
	return r, nil
}

// Review applies a human decision.
func (s *Service) Review(ctx context.Context, id string, rv workflow.Review) (*workflow.Request, error) {
	ctx, span := s.tracer.Start(ctx, "app.Review", trace.WithAttributes(attribute.String("request.id", id), attribute.String("review.action", string(rv.Action))))
	defer span.End()
	r, err := s.d.Repo.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.d.Clock()
	evs, err := r.Review(rv, now)
	if err != nil {
		return nil, err
	}
	if err := s.commit(ctx, r, evs, rv.Actor, now); err != nil {
		return nil, err
	}
	s.d.Logger.InfoContext(ctx, "review recorded", "request_id", r.ID, "action", rv.Action, "actor", rv.Actor, "state", r.State)
	return r, nil
}

// Sync inquires the payer about one submitted/pended/appealed request.
func (s *Service) Sync(ctx context.Context, id string) (*workflow.Request, error) {
	r, err := s.d.Repo.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.Submission == nil || r.Submission.PayerRef == "" {
		return nil, fmt.Errorf("%w: no payer reference to inquire about", workflow.ErrInvalidTransition)
	}
	switch r.State {
	case workflow.StateSubmitted, workflow.StatePended, workflow.StateAppealed:
	default:
		return nil, fmt.Errorf("%w: cannot sync in state %s", workflow.ErrInvalidTransition, r.State)
	}
	now := s.d.Clock()
	start := time.Now()
	resp, err := s.d.Gateway.Inquire(ctx, r.Submission.PayerRef)
	s.d.Metrics.PayerLatency.WithLabelValues("inquire", resultLabel(err)).Observe(time.Since(start).Seconds())
	if err != nil {
		return r, fmt.Errorf("%w: %w", ErrPayerUnavailable, err)
	}
	decision, err := pas.Interpret(resp, now)
	if err != nil {
		evs := r.RecordInvalidPayerResponse(err.Error(), now)
		if cerr := s.commit(ctx, r, evs, ActorEngine, now); cerr != nil {
			return nil, cerr
		}
		return r, nil
	}
	evs, err := r.RecordPayerDecision(decision, now)
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return r, nil
	}
	s.d.Metrics.PayerDecisions.WithLabelValues(string(decision.Outcome)).Inc()
	if err := s.commit(ctx, r, evs, ActorEngine, now); err != nil {
		return nil, err
	}
	s.d.Logger.InfoContext(ctx, "payer status synced", "request_id", r.ID, "outcome", decision.Outcome, "state", r.State)
	return r, nil
}

// PollPended syncs every pended (or appeal-submitted) request. It is run
// periodically by the background loop and returns the number synced.
func (s *Service) PollPended(ctx context.Context) (int, error) {
	pended, err := s.d.Repo.List(ctx, ports.Filter{State: workflow.StatePended})
	if err != nil {
		return 0, err
	}
	appealed, err := s.d.Repo.List(ctx, ports.Filter{State: workflow.StateAppealed})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range append(pended, appealed...) {
		if r.Submission == nil || r.Submission.PayerRef == "" || (r.State == workflow.StateAppealed && !r.Submission.Appeal) {
			continue
		}
		if _, err := s.Sync(ctx, r.ID); err != nil {
			s.d.Logger.WarnContext(ctx, "poll: sync failed", "request_id", r.ID, "err", err)
			continue
		}
		n++
	}
	return n, nil
}

// CheckSLAs breaches every due timer on active requests. Returns the number
// of breaches recorded.
func (s *Service) CheckSLAs(ctx context.Context) (int, error) {
	active, err := s.d.Repo.List(ctx, ports.Filter{Active: true})
	if err != nil {
		return 0, err
	}
	now := s.d.Clock()
	n := 0
	for _, view := range active {
		due := s.d.SLA.Due(view, now)
		if len(due) == 0 {
			continue
		}
		r, err := s.d.Repo.Load(ctx, view.ID)
		if err != nil {
			s.d.Logger.WarnContext(ctx, "sla: load failed", "request_id", view.ID, "err", err)
			continue
		}
		var evs []workflow.Event
		for _, d := range due {
			evs = append(evs, r.Breach(d.Timer, d.DueAt, now)...)
			s.d.Metrics.SLABreaches.WithLabelValues(string(d.Timer)).Inc()
		}
		if len(evs) == 0 {
			continue
		}
		if err := s.commit(ctx, r, evs, ActorEngine, now); err != nil {
			s.d.Logger.WarnContext(ctx, "sla: commit failed", "request_id", r.ID, "err", err)
			continue
		}
		n++
		s.d.Logger.WarnContext(ctx, "sla breached", "request_id", r.ID, "timers", due, "state", r.State)
	}
	return n, nil
}

// ExceptionItem is one row of the exceptions queue.
type ExceptionItem struct {
	RequestID   string             `json:"request_id"`
	State       workflow.State     `json:"state"`
	Payer       string             `json:"payer"`
	ServiceCode string             `json:"service_code"`
	PatientID   string             `json:"patient_id"`
	Urgent      bool               `json:"urgent,omitempty"`
	Exception   workflow.Exception `json:"exception"`
	Age         string             `json:"age"`
	// SuggestedAction tells the reviewer which review action clears it.
	SuggestedAction string `json:"suggested_action"`
}

// Exceptions returns the work queue ordered by severity then age (oldest
// first). It also refreshes the open-exceptions gauge.
func (s *Service) Exceptions(ctx context.Context) ([]ExceptionItem, error) {
	active, err := s.d.Repo.List(ctx, ports.Filter{Active: true})
	if err != nil {
		return nil, err
	}
	now := s.d.Clock()
	var out []ExceptionItem
	open := map[string]int{}
	for _, r := range active {
		for _, x := range r.Exceptions {
			open[x.Code]++
			out = append(out, ExceptionItem{
				RequestID: r.ID, State: r.State, Payer: r.Intake.Payer.ID, ServiceCode: r.Intake.Service.Code,
				PatientID: r.Intake.Patient.ID, Urgent: r.Intake.Service.Urgent, Exception: x,
				Age:             now.Sub(x.RaisedAt).Truncate(time.Second).String(),
				SuggestedAction: suggest(x.Code, r.State),
			})
		}
	}
	s.d.Metrics.ExceptionsOpen.Reset()
	for code, n := range open {
		s.d.Metrics.ExceptionsOpen.WithLabelValues(code).Set(float64(n))
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := severityRank(out[i].Exception.Severity), severityRank(out[j].Exception.Severity)
		if si != sj {
			return si < sj
		}
		if out[i].Urgent != out[j].Urgent {
			return out[i].Urgent
		}
		return out[i].Exception.RaisedAt.Before(out[j].Exception.RaisedAt)
	})
	return out, nil
}

func suggest(code string, state workflow.State) string {
	switch {
	case code == workflow.ExcIndeterminate:
		return "review: set_determination"
	case code == workflow.ExcEvidenceReview:
		return "review: confirm_evidence / reject_evidence, then assemble"
	case code == workflow.ExcCriteriaNotMet, code == workflow.ExcCriteriaUnk:
		return "add evidence and assemble, or review: override_criteria"
	case code == workflow.ExcDocsMissing:
		return "attach documentation and assemble"
	case code == workflow.ExcDenied:
		return "review: appeal or withdraw"
	case code == workflow.ExcSubmitFailed:
		return "retry submit"
	case code == workflow.ExcPayerInvalid:
		return "inspect raw payer response; sync"
	case strings.HasPrefix(code, workflow.ExcSLAPrefix):
		return "escalate; request is in state " + string(state)
	}
	return "review"
}

func severityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "warning":
		return 1
	}
	return 2
}

// TurnaroundStats is the GET /v1/metrics/turnaround payload.
type TurnaroundStats struct {
	AsOf           time.Time                 `json:"as_of"`
	Total          int                       `json:"total"`
	InFlight       int                       `json:"in_flight"`
	OpenExceptions int                       `json:"open_exceptions"`
	ByState        map[workflow.State]int    `json:"by_state"`
	Decided        Percentiles               `json:"decided"`
	ByPayer        map[string]Percentiles    `json:"by_payer"`
	ByOutcome      map[string]Percentiles    `json:"by_outcome"`
	AutoDetermined float64                   `json:"auto_determined_ratio"` // determinations without human set_determination
	SLABreaches    map[workflow.Timer]int    `json:"sla_breaches"`
	Payer          map[string]map[string]int `json:"payer_outcomes"`
}

// Percentiles summarises durations in seconds.
type Percentiles struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_seconds"`
	P95   float64 `json:"p95_seconds"`
	Mean  float64 `json:"mean_seconds"`
	Max   float64 `json:"max_seconds"`
}

// Turnaround computes created→decided statistics over the read model.
func (s *Service) Turnaround(ctx context.Context) (TurnaroundStats, error) {
	all, err := s.d.Repo.List(ctx, ports.Filter{})
	if err != nil {
		return TurnaroundStats{}, err
	}
	now := s.d.Clock()
	st := TurnaroundStats{AsOf: now, ByState: map[workflow.State]int{}, ByPayer: map[string]Percentiles{}, ByOutcome: map[string]Percentiles{}, SLABreaches: map[workflow.Timer]int{}, Payer: map[string]map[string]int{}}
	var decided []float64
	byPayer := map[string][]float64{}
	byOutcome := map[string][]float64{}
	auto, determined := 0, 0
	for _, r := range all {
		st.Total++
		st.ByState[r.State]++
		st.OpenExceptions += len(r.Exceptions)
		for _, t := range r.Breached {
			st.SLABreaches[t]++
		}
		if r.Determination != nil {
			determined++
			if !strings.HasPrefix(r.Determination.RuleRef, "human:") {
				auto++
			}
		}
		if r.Decision != nil && r.Decision.Outcome != workflow.OutcomePended {
			if st.Payer[r.Intake.Payer.ID] == nil {
				st.Payer[r.Intake.Payer.ID] = map[string]int{}
			}
			st.Payer[r.Intake.Payer.ID][string(r.Decision.Outcome)]++
		}
		if r.DecidedAt.IsZero() {
			if !r.State.Terminal() {
				st.InFlight++
			}
			continue
		}
		d := r.DecidedAt.Sub(r.CreatedAt).Seconds()
		decided = append(decided, d)
		byPayer[r.Intake.Payer.ID] = append(byPayer[r.Intake.Payer.ID], d)
		byOutcome[outcomeOf(r)] = append(byOutcome[outcomeOf(r)], d)
	}
	st.Decided = percentiles(decided)
	for k, v := range byPayer {
		st.ByPayer[k] = percentiles(v)
	}
	for k, v := range byOutcome {
		st.ByOutcome[k] = percentiles(v)
	}
	if determined > 0 {
		st.AutoDetermined = float64(auto) / float64(determined)
	}
	return st, nil
}

func outcomeOf(r *workflow.Request) string {
	switch {
	case r.State == workflow.StateClosed:
		return "closed"
	case r.Decision != nil:
		return string(r.Decision.Outcome)
	case r.Determination != nil && r.Determination.Decision == rules.NotRequired:
		return "not_required"
	}
	return string(r.State)
}

func percentiles(v []float64) Percentiles {
	if len(v) == 0 {
		return Percentiles{}
	}
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, x := range sorted {
		sum += x
	}
	pick := func(p float64) float64 {
		i := int(p*float64(len(sorted)-1) + 0.5)
		return sorted[i]
	}
	return Percentiles{Count: len(sorted), P50: pick(0.5), P95: pick(0.95), Mean: sum / float64(len(sorted)), Max: sorted[len(sorted)-1]}
}

// staged accumulates envelopes across several commands on one aggregate
// so they commit in a single Save (one transaction, one version bump).
type staged struct {
	envs   []workflow.Envelope
	before workflow.State
	raised []string
}

// stage applies evs to the aggregate and wraps them for the store.
func (s *Service) stage(ctx context.Context, r *workflow.Request, evs []workflow.Event, actor string, now time.Time, st *staged) error {
	if len(st.envs) == 0 {
		st.before = r.State
	}
	envs, err := r.Emit(now, actor, evs)
	if err != nil {
		return err
	}
	tid := observability.TraceID(ctx)
	for i := range envs {
		envs[i].ID = s.d.NewID()
		envs[i].TraceID = tid
	}
	st.envs = append(st.envs, envs...)
	for _, ev := range evs {
		if x, ok := ev.(workflow.ExceptionRaised); ok {
			st.raised = append(st.raised, x.Exception.Code)
		}
	}
	return nil
}

// flush persists, publishes and records metrics for the staged envelopes.
func (s *Service) flush(ctx context.Context, r *workflow.Request, st *staged, now time.Time) error {
	if len(st.envs) == 0 {
		return nil
	}
	if err := s.d.Repo.Save(ctx, r, st.envs); err != nil {
		return fmt.Errorf("app: save %s: %w", r.ID, err)
	}
	if err := s.d.Publisher.Publish(ctx, st.envs); err != nil {
		// At-least-once semantics are provided by the event store (replay);
		// a publish failure is degraded delivery, not a failed command.
		s.d.Metrics.EventsPublished.WithLabelValues("error").Add(float64(len(st.envs)))
		s.d.Logger.ErrorContext(ctx, "publish failed", "request_id", r.ID, "events", len(st.envs), "err", err)
	} else {
		s.d.Metrics.EventsPublished.WithLabelValues("ok").Add(float64(len(st.envs)))
	}
	if r.State != st.before {
		s.d.Metrics.Transitions.WithLabelValues(string(r.State)).Inc()
	}
	for _, code := range st.raised {
		s.d.Metrics.ExceptionsTotal.WithLabelValues(code).Inc()
	}
	if !r.DecidedAt.IsZero() && r.DecidedAt.Equal(now) {
		s.d.Metrics.Turnaround.WithLabelValues(r.Intake.Payer.ID, outcomeOf(r)).Observe(r.DecidedAt.Sub(r.CreatedAt).Seconds())
	}
	return nil
}

// commit stages and flushes one batch of events.
func (s *Service) commit(ctx context.Context, r *workflow.Request, evs []workflow.Event, actor string, now time.Time) error {
	if len(evs) == 0 {
		return nil
	}
	var st staged
	if err := s.stage(ctx, r, evs, actor, now, &st); err != nil {
		return err
	}
	return s.flush(ctx, r, &st, now)
}

func (s *Service) input(r *workflow.Request, now time.Time, withNotes bool) evidence.Input {
	in := evidence.Input{Bundle: r.Intake.Bundle, BirthDate: r.Intake.Patient.BirthDate, Sex: r.Intake.Patient.Sex, AsOf: now}
	if withNotes {
		in.Notes = r.Intake.Notes
	}
	return in
}

func resultLabel(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// contentKey derives a stable idempotency key from the intake content.
func contentKey(in workflow.Intake) string {
	b, _ := json.Marshal(in) // validated struct; cannot fail
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}
