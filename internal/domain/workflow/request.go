package workflow

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/rules"
)

// State of the request lifecycle.
type State string

const (
	StateDraft      State = "draft"
	StateDetermined State = "determined"
	StateAssembled  State = "assembled"
	StateSubmitted  State = "submitted"
	StatePended     State = "pended"
	StateApproved   State = "approved"
	StateDenied     State = "denied"
	StateAppealed   State = "appealed"
	StateClosed     State = "closed"
)

// Terminal reports whether no further transitions are expected.
func (s State) Terminal() bool { return s == StateApproved || s == StateClosed }

// ErrInvalidTransition is returned when a guard rejects a command.
var ErrInvalidTransition = errors.New("invalid transition")

// ErrNotFound is returned for unknown request ids.
var ErrNotFound = errors.New("request not found")

func invalid(state State, cmd, why string) error {
	if why == "" {
		return fmt.Errorf("%w: cannot %s in state %s", ErrInvalidTransition, cmd, state)
	}
	return fmt.Errorf("%w: cannot %s in state %s: %s", ErrInvalidTransition, cmd, state, why)
}

// Request is the aggregate. It is rebuilt by folding events with Apply and
// is also the shape of the read-model document.
type Request struct {
	ID      string `json:"id"`
	Version int    `json:"version"` // number of events applied
	State   State  `json:"state"`
	Intake  Intake `json:"intake"`

	Determination *rules.Determination `json:"determination,omitempty"`
	Evidence      *Package             `json:"evidence,omitempty"`
	Submission    *Submission          `json:"submission,omitempty"`
	Decision      *PayerDecision       `json:"decision,omitempty"`

	Reviews    []Review    `json:"reviews,omitempty"`
	Exceptions []Exception `json:"exceptions,omitempty"` // open only
	Breached   []Timer     `json:"breached,omitempty"`
	Appeals    int         `json:"appeals,omitempty"`

	// ConfirmedFacts / RejectedFacts are LLM proposal IDs decided by humans.
	ConfirmedFacts map[string]string `json:"confirmed_facts,omitempty"` // id → actor
	RejectedFacts  map[string]string `json:"rejected_facts,omitempty"`
	Override       *Review           `json:"override,omitempty"`

	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	DeterminedAt time.Time `json:"determined_at,omitempty"`
	AssembledAt  time.Time `json:"assembled_at,omitempty"`
	SubmittedAt  time.Time `json:"submitted_at,omitempty"`
	DecidedAt    time.Time `json:"decided_at,omitempty"` // final approved/denied/closed
	ClosedReason string    `json:"closed_reason,omitempty"`
}

// Submission is the latest PAS submission.
type Submission struct {
	ClaimID     string    `json:"claim_id"`
	PayerRef    string    `json:"payer_ref,omitempty"`
	Attempt     int       `json:"attempt"`
	Appeal      bool      `json:"appeal,omitempty"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// Load rebuilds an aggregate from its envelopes.
func Load(envs []Envelope) (*Request, error) {
	if len(envs) == 0 {
		return nil, ErrNotFound
	}
	r := &Request{}
	for _, env := range envs {
		ev, err := Decode(env)
		if err != nil {
			return nil, err
		}
		r.Apply(env.At, ev)
		r.ID = env.RequestID
	}
	return r, nil
}

// Apply folds one event into the aggregate. It never fails: events in the
// store are facts, and rejecting one would make the stream unreadable.
func (r *Request) Apply(at time.Time, ev Event) {
	r.Version++
	r.UpdatedAt = at
	switch e := ev.(type) {
	case RequestCreated:
		r.Intake = e.Intake
		r.State = StateDraft
		r.CreatedAt = at
		r.ConfirmedFacts = map[string]string{}
		r.RejectedFacts = map[string]string{}
	case DeterminationRecorded:
		d := e.Determination
		r.Determination = &d
		r.State = StateDetermined
		r.DeterminedAt = at
		if d.Decision == rules.NotRequired {
			r.DecidedAt = at
		}
	case EvidenceAssembled:
		p := e.Package
		r.Evidence = &p
		r.State = StateAssembled
		r.AssembledAt = at
	case Submitted:
		r.Submission = &Submission{ClaimID: e.ClaimID, PayerRef: e.PayerRef, Attempt: e.Attempt, Appeal: e.Appeal, SubmittedAt: at}
		r.SubmittedAt = at
		if !e.Appeal {
			r.State = StateSubmitted
		}
	case PayerResponded:
		d := e.Decision
		r.Decision = &d
		if r.Submission != nil && d.PayerRef != "" {
			r.Submission.PayerRef = d.PayerRef
		}
		switch d.Outcome {
		case OutcomeApproved:
			r.State, r.DecidedAt = StateApproved, at
		case OutcomeDenied:
			r.State, r.DecidedAt = StateDenied, at
		case OutcomePended:
			// A pended appeal stays in Appealed: the appeal is what is pending.
			if r.Submission == nil || !r.Submission.Appeal {
				r.State = StatePended
			}
		}
	case Reviewed:
		r.Reviews = append(r.Reviews, e.Review)
		switch e.Review.Action {
		case ActionConfirmEvidence:
			for _, id := range e.Review.FactIDs {
				r.ConfirmedFacts[id] = e.Review.Actor
				delete(r.RejectedFacts, id)
			}
		case ActionRejectEvidence:
			for _, id := range e.Review.FactIDs {
				r.RejectedFacts[id] = e.Review.Actor
				delete(r.ConfirmedFacts, id)
			}
		case ActionOverrideCriteria:
			rv := e.Review
			r.Override = &rv
		case ActionSetDetermination:
			if r.Determination != nil {
				r.Determination.Decision = e.Review.Decision
				r.Determination.RuleRef = "human:" + e.Review.Actor
				if e.Review.Decision == rules.NotRequired {
					r.DecidedAt = at
				}
			}
		}
	case Appealed:
		r.Appeals++
		r.State = StateAppealed
		r.DecidedAt = time.Time{}
	case ExceptionRaised:
		r.Exceptions = append(r.Exceptions, e.Exception)
	case ExceptionResolved:
		kept := r.Exceptions[:0]
		for _, x := range r.Exceptions {
			if x.Code != e.Code {
				kept = append(kept, x)
			}
		}
		r.Exceptions = kept
	case SLABreached:
		r.Breached = append(r.Breached, e.Timer)
	case Closed:
		r.State = StateClosed
		r.ClosedReason = e.Reason
		r.DecidedAt = at
	}
}

// hasException reports whether code is open.
func (r *Request) hasException(code string) bool {
	for _, x := range r.Exceptions {
		if x.Code == code {
			return true
		}
	}
	return false
}

func (r *Request) hasBreached(t Timer) bool {
	for _, b := range r.Breached {
		if b == t {
			return true
		}
	}
	return false
}

// raise returns an ExceptionRaised event unless the code is already open.
func (r *Request) raise(at time.Time, code, detail, severity string) []Event {
	if r.hasException(code) {
		return nil
	}
	return []Event{ExceptionRaised{Exception: Exception{Code: code, Detail: detail, Severity: severity, RaisedAt: at}}}
}

func (r *Request) resolve(code, reason string) []Event {
	if !r.hasException(code) {
		return nil
	}
	return []Event{ExceptionResolved{Code: code, Reason: reason}}
}

// ---------------------------------------------------------------------------
// Commands. Each validates guards against the current state and returns the
// events to append. The caller applies them (see Emit).

// Create opens a new request.
func Create(id string, in Intake, at time.Time) (*Request, []Event, error) {
	if err := in.Validate(); err != nil {
		return nil, nil, err
	}
	r := &Request{ID: id}
	evs := []Event{RequestCreated{Intake: in}}
	return r, evs, nil
}

// RecordDetermination stores a rule-engine result. Allowed from Draft and
// Determined (re-run after a rules reload); re-running after assembly would
// silently invalidate the evidence package, so it is rejected.
func (r *Request) RecordDetermination(d rules.Determination, at time.Time) ([]Event, error) {
	if r.State != StateDraft && r.State != StateDetermined {
		return nil, invalid(r.State, "determine", "determination is fixed once evidence is assembled")
	}
	evs := []Event{DeterminationRecorded{Determination: d}}
	switch d.Decision {
	case rules.Indeterminate:
		evs = append(evs, r.raise(at, ExcIndeterminate, strings.Join(d.MissingData, "; "), "warning")...)
	default:
		evs = append(evs, r.resolve(ExcIndeterminate, "rule matched: "+d.RuleRef)...)
	}
	return evs, nil
}

// CanAssemble reports whether the Assemble guard passes: Determined
// (Required) or Assembled (re-assembly after evidence review or new data).
func (r *Request) CanAssemble() error {
	switch r.State {
	case StateDetermined:
		if r.Determination == nil || r.Determination.Decision != rules.Required {
			return invalid(r.State, "assemble", "authorization is not required")
		}
		return nil
	case StateAssembled:
		return nil
	}
	return invalid(r.State, "assemble", "")
}

// RecordEvidence stores an assembled package.
func (r *Request) RecordEvidence(p Package, at time.Time) ([]Event, error) {
	if err := r.CanAssemble(); err != nil {
		return nil, err
	}
	evs := []Event{EvidenceAssembled{Package: p}}
	// Exceptions mirror the package state exactly, so re-assembly resolves
	// what it fixes and raises what it finds.
	switch p.Criteria {
	case rules.Met:
		evs = append(evs, r.resolve(ExcCriteriaNotMet, "criteria met")...)
		evs = append(evs, r.resolve(ExcCriteriaUnk, "criteria met")...)
	case rules.NotMet:
		evs = append(evs, r.resolve(ExcCriteriaUnk, "criteria evaluated")...)
		if r.Override == nil {
			evs = append(evs, r.raise(at, ExcCriteriaNotMet, "clinical criteria not met; clinician override or additional evidence needed", "critical")...)
		}
	case rules.Unknown:
		evs = append(evs, r.resolve(ExcCriteriaNotMet, "criteria re-evaluated")...)
		if r.Override == nil {
			evs = append(evs, r.raise(at, ExcCriteriaUnk, "insufficient data: "+strings.Join(p.MissingData, "; "), "warning")...)
		}
	}
	if p.Proposals > 0 {
		evs = append(evs, r.raise(at, ExcEvidenceReview, fmt.Sprintf("%d extracted evidence proposal(s) await confirmation", p.Proposals), "info")...)
	} else {
		evs = append(evs, r.resolve(ExcEvidenceReview, "no proposals pending")...)
	}
	var missing []string
	for _, d := range p.Documentation {
		if !d.Optional && !d.Satisfied {
			missing = append(missing, d.Code)
		}
	}
	if len(missing) > 0 {
		evs = append(evs, r.raise(at, ExcDocsMissing, "missing: "+strings.Join(missing, ", "), "warning")...)
	} else {
		evs = append(evs, r.resolve(ExcDocsMissing, "documentation complete")...)
	}
	return evs, nil
}

// CanSubmit reports whether the Submit guard passes, with the reason if not.
func (r *Request) CanSubmit() error {
	switch r.State {
	case StateAssembled:
		if r.Evidence == nil {
			return invalid(r.State, "submit", "no evidence package")
		}
		if !r.Evidence.Complete {
			return invalid(r.State, "submit", "evidence package incomplete (criteria unmet or documentation missing)")
		}
		return nil
	case StateAppealed:
		if r.Submission != nil && r.Submission.Appeal {
			return invalid(r.State, "submit", "appeal already submitted; awaiting payer")
		}
		return nil
	}
	return invalid(r.State, "submit", "")
}

// MarkSubmitted records a successful send. Attempt numbers increase.
func (r *Request) MarkSubmitted(claimID, payerRef string, bundle []byte, at time.Time) ([]Event, error) {
	if err := r.CanSubmit(); err != nil {
		return nil, err
	}
	attempt := 1
	if r.Submission != nil {
		attempt = r.Submission.Attempt + 1
	}
	resolved := r.resolve(ExcSubmitFailed, "submitted")
	evs := make([]Event, 0, 1+len(resolved))
	evs = append(evs, Submitted{ClaimID: claimID, PayerRef: payerRef, Attempt: attempt, Appeal: r.State == StateAppealed, Bundle: bundle})
	evs = append(evs, resolved...)
	return evs, nil
}

// SubmissionFailed records a transport failure as an exception without
// changing state, so the request stays retryable.
func (r *Request) SubmissionFailed(reason string, at time.Time) ([]Event, error) {
	if err := r.CanSubmit(); err != nil {
		return nil, err
	}
	return r.raise(at, ExcSubmitFailed, reason, "critical"), nil
}

// RecordInvalidPayerResponse flags a payer answer the engine could not
// interpret. State is unchanged; a human must look at the raw response.
func (r *Request) RecordInvalidPayerResponse(reason string, at time.Time) []Event {
	return r.raise(at, ExcPayerInvalid, reason, "critical")
}

// RecordPayerDecision applies a ClaimResponse. Allowed while Submitted,
// Pended (status update) or Appealed (appeal outcome).
func (r *Request) RecordPayerDecision(d PayerDecision, at time.Time) ([]Event, error) {
	switch r.State {
	case StateSubmitted, StatePended, StateAppealed:
	default:
		return nil, invalid(r.State, "record payer decision", "")
	}
	if d.Outcome == OutcomePended && r.Decision != nil && r.Decision.Outcome == OutcomePended && (d.PayerRef == "" || d.PayerRef == r.Decision.PayerRef) {
		return nil, nil // still pended: nothing new happened
	}
	if d.ReceivedAt.IsZero() {
		d.ReceivedAt = at
	}
	evs := []Event{PayerResponded{Decision: d}}
	evs = append(evs, r.resolve(ExcPayerInvalid, "payer response interpreted")...)
	switch d.Outcome {
	case OutcomeDenied:
		evs = append(evs, r.raise(at, ExcDenied, "payer denied: "+strings.Join(d.Reasons, "; "), "critical")...)
	case OutcomeApproved:
		evs = append(evs, r.resolve(ExcDenied, "approved")...)
		for _, x := range r.Exceptions {
			if strings.HasPrefix(x.Code, ExcSLAPrefix) {
				evs = append(evs, ExceptionResolved{Code: x.Code, Reason: "approved"})
			}
		}
	}
	return evs, nil
}

// Review applies a human decision.
func (r *Request) Review(rv Review, at time.Time) ([]Event, error) {
	if strings.TrimSpace(rv.Actor) == "" {
		return nil, fmt.Errorf("%w: review.actor is required", ErrValidation)
	}
	if r.State == StateClosed {
		return nil, invalid(r.State, "review", "request is closed")
	}
	rv.At = at
	var evs []Event
	switch rv.Action {
	case ActionConfirmEvidence, ActionRejectEvidence:
		if len(rv.FactIDs) == 0 {
			return nil, fmt.Errorf("%w: fact_ids required for %s", ErrValidation, rv.Action)
		}
		if r.Evidence == nil {
			return nil, invalid(r.State, string(rv.Action), "no evidence package to review")
		}
		known := map[string]bool{}
		for _, f := range r.Evidence.Facts {
			if f.Prov.Origin == evidence.OriginLLM {
				known[f.ID] = true
			}
		}
		for _, id := range rv.FactIDs {
			if !known[id] {
				return nil, fmt.Errorf("%w: fact %q is not an extracted proposal on this request", ErrValidation, id)
			}
		}
		evs = append(evs, Reviewed{Review: rv})
		// If every proposal is now decided, the review exception closes;
		// the package itself is refreshed by the next assemble.
		decided := len(rv.FactIDs)
		for id := range r.ConfirmedFacts {
			if !contains(rv.FactIDs, id) {
				decided++
			}
		}
		for id := range r.RejectedFacts {
			if !contains(rv.FactIDs, id) {
				decided++
			}
		}
		if decided >= len(known) {
			evs = append(evs, r.resolve(ExcEvidenceReview, "all proposals reviewed by "+rv.Actor)...)
		}
	case ActionOverrideCriteria:
		if len(strings.TrimSpace(rv.Reason)) < 10 {
			return nil, fmt.Errorf("%w: override_criteria needs a reason (>= 10 chars)", ErrValidation)
		}
		if r.State != StateDetermined && r.State != StateAssembled {
			return nil, invalid(r.State, "override criteria", "")
		}
		evs = append(evs, Reviewed{Review: rv})
		evs = append(evs, r.resolve(ExcCriteriaNotMet, "clinician override by "+rv.Actor)...)
		evs = append(evs, r.resolve(ExcCriteriaUnk, "clinician override by "+rv.Actor)...)
	case ActionSetDetermination:
		if rv.Decision != rules.Required && rv.Decision != rules.NotRequired {
			return nil, fmt.Errorf("%w: decision must be required|not_required", ErrValidation)
		}
		if r.State != StateDetermined || r.Determination == nil || r.Determination.Decision != rules.Indeterminate {
			return nil, invalid(r.State, "set determination", "only an indeterminate determination can be set by hand")
		}
		evs = append(evs, Reviewed{Review: rv})
		evs = append(evs, r.resolve(ExcIndeterminate, "decided by "+rv.Actor)...)
	case ActionAppeal:
		if r.State != StateDenied {
			return nil, invalid(r.State, "appeal", "only a denied request can be appealed")
		}
		if strings.TrimSpace(rv.Reason) == "" {
			return nil, fmt.Errorf("%w: appeal needs a reason", ErrValidation)
		}
		evs = append(evs, Reviewed{Review: rv}, Appealed{Reason: rv.Reason, Actor: rv.Actor})
		evs = append(evs, r.resolve(ExcDenied, "appealed by "+rv.Actor)...)
	case ActionWithdraw:
		if r.State.Terminal() {
			return nil, invalid(r.State, "withdraw", "")
		}
		evs = append(evs, Reviewed{Review: rv}, Closed{Reason: "withdrawn: " + rv.Reason, Actor: rv.Actor})
		for _, x := range r.Exceptions {
			evs = append(evs, ExceptionResolved{Code: x.Code, Reason: "withdrawn"})
		}
	default:
		return nil, fmt.Errorf("%w: unknown review action %q", ErrValidation, rv.Action)
	}
	return evs, nil
}

// Breach records an SLA timer expiry once and raises an exception.
func (r *Request) Breach(t Timer, deadline, at time.Time) []Event {
	if r.hasBreached(t) || r.State.Terminal() {
		return nil
	}
	raised := r.raise(at, ExcSLAPrefix+string(t), fmt.Sprintf("%s SLA missed (deadline %s)", t, deadline.UTC().Format(time.RFC3339)), "warning")
	evs := make([]Event, 0, 1+len(raised))
	evs = append(evs, SLABreached{Timer: t, Deadline: deadline})
	evs = append(evs, raised...)
	return evs
}

// Emit applies events to the aggregate and returns them wrapped in
// envelopes ready for the store, with sequence numbers continuing from the
// current version.
func (r *Request) Emit(at time.Time, actor string, evs []Event) ([]Envelope, error) {
	out := make([]Envelope, 0, len(evs))
	for _, ev := range evs {
		env, err := Wrap(r.ID, r.Version+1, at, actor, ev)
		if err != nil {
			return nil, err
		}
		r.Apply(at, ev)
		out = append(out, env)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// SLA timers.

// SLAPolicy holds the deadlines per timer; zero disables a timer.
type SLAPolicy struct {
	Determine time.Duration
	Assemble  time.Duration
	Submit    time.Duration
	Response  time.Duration
	Pended    time.Duration
	Review    time.Duration
	// UrgentFactor scales every deadline for urgent requests (e.g. 0.25).
	UrgentFactor float64
}

// Deadline is a pending timer.
type Deadline struct {
	Timer Timer
	DueAt time.Time
}

// Deadlines returns the timers currently running for the request, in due
// order. Already-breached timers are excluded so each fires once.
func (p SLAPolicy) Deadlines(r *Request) []Deadline {
	if r.State.Terminal() {
		return nil
	}
	scale := func(d time.Duration) time.Duration {
		if r.Intake.Service.Urgent && p.UrgentFactor > 0 {
			return time.Duration(float64(d) * p.UrgentFactor)
		}
		return d
	}
	var out []Deadline
	add := func(t Timer, from time.Time, d time.Duration) {
		if d <= 0 || from.IsZero() || r.hasBreached(t) {
			return
		}
		out = append(out, Deadline{Timer: t, DueAt: from.Add(scale(d))})
	}
	switch r.State {
	case StateDraft:
		add(TimerDetermine, r.CreatedAt, p.Determine)
	case StateDetermined:
		if r.Determination != nil && r.Determination.Decision == rules.Required {
			add(TimerAssemble, r.DeterminedAt, p.Assemble)
		}
	case StateAssembled:
		add(TimerSubmit, r.AssembledAt, p.Submit)
	case StateSubmitted:
		add(TimerResponse, r.SubmittedAt, p.Response)
	case StatePended:
		if r.Decision != nil {
			add(TimerPended, r.Decision.ReceivedAt, p.Pended)
		}
	case StateAppealed:
		if r.Submission != nil && r.Submission.Appeal {
			add(TimerResponse, r.SubmittedAt, p.Response)
		}
	}
	if len(r.Exceptions) > 0 && p.Review > 0 && !r.hasBreached(TimerReview) {
		oldest := r.Exceptions[0].RaisedAt
		for _, x := range r.Exceptions {
			if x.RaisedAt.Before(oldest) {
				oldest = x.RaisedAt
			}
		}
		add(TimerReview, oldest, p.Review)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DueAt.Before(out[j].DueAt) })
	return out
}

// Due returns the timers whose deadline has passed at now.
func (p SLAPolicy) Due(r *Request, now time.Time) []Deadline {
	var out []Deadline
	for _, d := range p.Deadlines(r) {
		if !now.Before(d.DueAt) {
			out = append(out, d)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
