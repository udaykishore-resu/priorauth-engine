package workflow

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/rules"
)

// Event is a domain fact that happened to a request.
type Event interface{ EventType() string }

// Event type names. They are part of the persisted contract; never rename.
const (
	TypeRequestCreated        = "request.created"
	TypeDeterminationRecorded = "determination.recorded"
	TypeEvidenceAssembled     = "evidence.assembled"
	TypeSubmitted             = "submission.sent"
	TypePayerResponded        = "payer.responded"
	TypeReviewed              = "review.recorded"
	TypeAppealed              = "appeal.filed"
	TypeExceptionRaised       = "exception.raised"
	TypeExceptionResolved     = "exception.resolved"
	TypeSLABreached           = "sla.breached"
	TypeClosed                = "request.closed"
)

// RequestCreated opens the stream.
type RequestCreated struct {
	Intake Intake `json:"intake"`
}

// DeterminationRecorded stores the rule engine output.
type DeterminationRecorded struct {
	Determination rules.Determination `json:"determination"`
}

// EvidenceAssembled stores the evidence package.
type EvidenceAssembled struct {
	Package Package `json:"package"`
}

// Submitted records a PAS Claim sent to the payer.
type Submitted struct {
	ClaimID  string          `json:"claim_id"`
	PayerRef string          `json:"payer_ref,omitempty"`
	Attempt  int             `json:"attempt"`
	Appeal   bool            `json:"appeal,omitempty"`
	Bundle   json.RawMessage `json:"bundle,omitempty"` // the PAS request Bundle, for audit
}

// PayerResponded records a ClaimResponse interpretation.
type PayerResponded struct {
	Decision PayerDecision `json:"decision"`
}

// Reviewed records a human decision.
type Reviewed struct {
	Review Review `json:"review"`
}

// Appealed records an appeal against a denial.
type Appealed struct {
	Reason string `json:"reason"`
	Actor  string `json:"actor"`
}

// ExceptionRaised puts the request on the exceptions queue.
type ExceptionRaised struct {
	Exception Exception `json:"exception"`
}

// ExceptionResolved removes an exception code from the queue.
type ExceptionResolved struct {
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
}

// SLABreached records a timer expiry.
type SLABreached struct {
	Timer    Timer     `json:"timer"`
	Deadline time.Time `json:"deadline"`
}

// Closed terminates the request.
type Closed struct {
	Reason string `json:"reason"`
	Actor  string `json:"actor,omitempty"`
}

func (RequestCreated) EventType() string        { return TypeRequestCreated }
func (DeterminationRecorded) EventType() string { return TypeDeterminationRecorded }
func (EvidenceAssembled) EventType() string     { return TypeEvidenceAssembled }
func (Submitted) EventType() string             { return TypeSubmitted }
func (PayerResponded) EventType() string        { return TypePayerResponded }
func (Reviewed) EventType() string              { return TypeReviewed }
func (Appealed) EventType() string              { return TypeAppealed }
func (ExceptionRaised) EventType() string       { return TypeExceptionRaised }
func (ExceptionResolved) EventType() string     { return TypeExceptionResolved }
func (SLABreached) EventType() string           { return TypeSLABreached }
func (Closed) EventType() string                { return TypeClosed }

// Envelope is the persisted form of an event.
type Envelope struct {
	ID        string          `json:"id"`
	RequestID string          `json:"request_id"`
	Seq       int             `json:"seq"` // 1-based position in the stream
	Type      string          `json:"type"`
	At        time.Time       `json:"at"`
	Actor     string          `json:"actor,omitempty"`
	TraceID   string          `json:"trace_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

// Wrap serialises an event into an envelope.
func Wrap(requestID string, seq int, at time.Time, actor string, ev Event) (Envelope, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return Envelope{}, fmt.Errorf("workflow: marshal %s: %w", ev.EventType(), err)
	}
	return Envelope{RequestID: requestID, Seq: seq, Type: ev.EventType(), At: at, Actor: actor, Payload: b}, nil
}

// Decode turns an envelope back into a typed event.
func Decode(env Envelope) (Event, error) {
	var ev Event
	switch env.Type {
	case TypeRequestCreated:
		ev = &RequestCreated{}
	case TypeDeterminationRecorded:
		ev = &DeterminationRecorded{}
	case TypeEvidenceAssembled:
		ev = &EvidenceAssembled{}
	case TypeSubmitted:
		ev = &Submitted{}
	case TypePayerResponded:
		ev = &PayerResponded{}
	case TypeReviewed:
		ev = &Reviewed{}
	case TypeAppealed:
		ev = &Appealed{}
	case TypeExceptionRaised:
		ev = &ExceptionRaised{}
	case TypeExceptionResolved:
		ev = &ExceptionResolved{}
	case TypeSLABreached:
		ev = &SLABreached{}
	case TypeClosed:
		ev = &Closed{}
	default:
		return nil, fmt.Errorf("workflow: unknown event type %q (seq %d)", env.Type, env.Seq)
	}
	if err := json.Unmarshal(env.Payload, ev); err != nil {
		return nil, fmt.Errorf("workflow: decode %s seq %d: %w", env.Type, env.Seq, err)
	}
	return deref(ev), nil
}

// deref returns the value type so Apply's type switch can use values.
func deref(ev Event) Event {
	switch e := ev.(type) {
	case *RequestCreated:
		return *e
	case *DeterminationRecorded:
		return *e
	case *EvidenceAssembled:
		return *e
	case *Submitted:
		return *e
	case *PayerResponded:
		return *e
	case *Reviewed:
		return *e
	case *Appealed:
		return *e
	case *ExceptionRaised:
		return *e
	case *ExceptionResolved:
		return *e
	case *SLABreached:
		return *e
	case *Closed:
		return *e
	}
	return ev
}

// Package is the assembled evidence bundle that accompanies a submission.
type Package struct {
	Facts         evidence.Set  `json:"facts"`
	Criteria      rules.Outcome `json:"criteria"`
	Explain       *rules.Node   `json:"explain,omitempty"`
	MissingData   []string      `json:"missing_data,omitempty"`
	Documentation []DocStatus   `json:"documentation"`
	// Proposals counts LLM facts awaiting human confirmation.
	Proposals int `json:"proposals"`
	// Complete is true when criteria are met (or overridden) and all required
	// documentation is present: the guard for Submit.
	Complete   bool     `json:"complete"`
	Extractors []string `json:"extractors"`
	// Warnings records extractor failures (e.g. LLM endpoint down) so a
	// reviewer knows the package may be thinner than it could be.
	Warnings    []string  `json:"warnings,omitempty"`
	AssembledAt time.Time `json:"assembled_at"`
}

// DocStatus is the documentation checklist entry.
type DocStatus struct {
	Code      string `json:"code"`
	Display   string `json:"display"`
	Optional  bool   `json:"optional,omitempty"`
	Satisfied bool   `json:"satisfied"`
	Source    string `json:"source,omitempty"`
}

// PayerOutcome is the interpreted payer decision.
type PayerOutcome string

const (
	OutcomeApproved PayerOutcome = "approved"
	OutcomeDenied   PayerOutcome = "denied"
	OutcomePended   PayerOutcome = "pended"
)

// PayerDecision is the domain view of a ClaimResponse.
type PayerDecision struct {
	Outcome    PayerOutcome    `json:"outcome"`
	PayerRef   string          `json:"payer_ref,omitempty"`
	AuthNumber string          `json:"auth_number,omitempty"`
	Reasons    []string        `json:"reasons,omitempty"`
	ValidFrom  time.Time       `json:"valid_from,omitempty"`
	ValidTo    time.Time       `json:"valid_to,omitempty"`
	ReceivedAt time.Time       `json:"received_at"`
	Raw        json.RawMessage `json:"raw,omitempty"` // ClaimResponse as received
}

// ReviewAction enumerates human decisions.
type ReviewAction string

const (
	// ActionConfirmEvidence accepts LLM proposals (by fact ID).
	ActionConfirmEvidence ReviewAction = "confirm_evidence"
	// ActionRejectEvidence discards LLM proposals (by fact ID).
	ActionRejectEvidence ReviewAction = "reject_evidence"
	// ActionOverrideCriteria lets a clinician assert medical necessity
	// despite unmet/unknown criteria; a reason is mandatory.
	ActionOverrideCriteria ReviewAction = "override_criteria"
	// ActionSetDetermination resolves an Indeterminate determination.
	ActionSetDetermination ReviewAction = "set_determination"
	// ActionAppeal files an appeal against a denial.
	ActionAppeal ReviewAction = "appeal"
	// ActionWithdraw closes the request.
	ActionWithdraw ReviewAction = "withdraw"
)

// Review is a human decision.
type Review struct {
	Action  ReviewAction `json:"action"`
	Actor   string       `json:"actor"`
	Reason  string       `json:"reason,omitempty"`
	FactIDs []string     `json:"fact_ids,omitempty"`
	// Decision is used by set_determination: "required" | "not_required".
	Decision rules.Decision `json:"decision,omitempty"`
	At       time.Time      `json:"at"`
}

// Exception codes.
const (
	ExcIndeterminate  = "indeterminate_rule"
	ExcCriteriaNotMet = "criteria_not_met"
	ExcCriteriaUnk    = "criteria_unknown"
	ExcEvidenceReview = "evidence_review"
	ExcDocsMissing    = "documentation_missing"
	ExcDenied         = "payer_denied"
	ExcSubmitFailed   = "submission_failed"
	ExcPayerInvalid   = "payer_response_invalid"
	ExcSLAPrefix      = "sla_"
)

// Exception is an item on the human work queue.
type Exception struct {
	Code     string    `json:"code"`
	Detail   string    `json:"detail"`
	Severity string    `json:"severity"` // info|warning|critical
	RaisedAt time.Time `json:"raised_at"`
}

// Timer names the SLA clocks.
type Timer string

const (
	TimerDetermine Timer = "determine" // Draft too long
	TimerAssemble  Timer = "assemble"  // Determined(Required) without evidence
	TimerSubmit    Timer = "submit"    // Assembled but not sent
	TimerResponse  Timer = "response"  // Submitted without payer answer
	TimerPended    Timer = "pended"    // Pended too long
	TimerReview    Timer = "review"    // exception open too long
)
