package workflow

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/rules"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func validIntake() Intake {
	return Intake{
		Patient:  Patient{ID: "pat-1", MemberID: "M123", BirthDate: time.Date(1971, 5, 14, 0, 0, 0, 0, time.UTC), Sex: "female"},
		Payer:    Payer{ID: "ACME_HEALTH", Plan: "HMO-SILVER"},
		Provider: Provider{NPI: "1234567893", Name: "Dr. Spine"},
		Service:  Service{Code: "72148", Diagnoses: []string{"M54.16"}, ServiceDate: t0.AddDate(0, 0, 14)},
		Attachments: []Attachment{
			{Type: "clinical_note", Ref: "DocumentReference/1"},
		},
	}
}

func newRequest(t *testing.T) *Request {
	t.Helper()
	r, evs, err := Create("req-1", validIntake(), t0)
	require.NoError(t, err)
	_, err = r.Emit(t0, "system", evs)
	require.NoError(t, err)
	require.Equal(t, StateDraft, r.State)
	return r
}

// do applies a command result to the aggregate, failing the test on error.
func do(t *testing.T, r *Request) func(evs []Event, err error) {
	t.Helper()
	return func(evs []Event, err error) {
		t.Helper()
		require.NoError(t, err)
		_, err = r.Emit(t0, "test", evs)
		require.NoError(t, err)
	}
}

func required(criteria rules.Outcome) rules.Determination {
	return rules.Determination{Decision: rules.Required, RuleRef: "rule@1", Criteria: criteria, Documentation: []rules.DocRequirement{{Code: "clinical_note", Display: "note"}}}
}

func completePackage() Package {
	return Package{Criteria: rules.Met, Complete: true, Documentation: []DocStatus{{Code: "clinical_note", Satisfied: true}}}
}

func TestIntakeValidate(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Intake)
		err  string
	}{
		{"ok", func(*Intake) {}, ""},
		{"plan defaults to wildcard", func(i *Intake) { i.Payer.Plan = "" }, ""},
		{"missing patient", func(i *Intake) { i.Patient.ID = "" }, "patient.id"},
		{"bad npi", func(i *Intake) { i.Provider.NPI = "12" }, "npi must be 10 digits"},
		{"no diagnoses", func(i *Intake) { i.Service.Diagnoses = nil }, "diagnoses"},
		{"no service date", func(i *Intake) { i.Service.ServiceDate = time.Time{} }, "service_date"},
		{"attachment without ref", func(i *Intake) { i.Attachments = []Attachment{{Type: "x"}} }, "attachments[0]"},
		{"empty note", func(i *Intake) { i.Notes = []evidence.Note{{ID: "n"}} }, "notes[0]"},
		{"invalid bundle", func(i *Intake) { i.Bundle = json.RawMessage(`{`) }, "fhir_bundle"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validIntake()
			tc.mut(&in)
			err := in.Validate()
			if tc.err == "" {
				require.NoError(t, err)
				require.NotEmpty(t, in.Payer.Plan)
				require.Equal(t, evidence.SystemCPT, in.Service.System)
				require.Equal(t, 1, in.Service.Units)
				return
			}
			require.ErrorIs(t, err, ErrValidation)
			require.ErrorContains(t, err, tc.err)
		})
	}
}

func TestHappyPath_ApprovedAfterPend(t *testing.T) {
	r := newRequest(t)

	do(t, r)(r.RecordDetermination(required(rules.Met), t0))
	require.Equal(t, StateDetermined, r.State)
	require.Empty(t, r.Exceptions)

	do(t, r)(r.RecordEvidence(completePackage(), t0))
	require.Equal(t, StateAssembled, r.State)
	require.NoError(t, r.CanSubmit())

	do(t, r)(r.MarkSubmitted("claim-1", "", []byte(`{}`), t0))
	require.Equal(t, StateSubmitted, r.State)
	require.Equal(t, 1, r.Submission.Attempt)

	do(t, r)(r.RecordPayerDecision(PayerDecision{Outcome: OutcomePended, PayerRef: "P-9"}, t0))
	require.Equal(t, StatePended, r.State)
	require.Equal(t, "P-9", r.Submission.PayerRef)

	// A repeated "still pended" poll is a no-op.
	evs, err := r.RecordPayerDecision(PayerDecision{Outcome: OutcomePended}, t0)
	require.NoError(t, err)
	require.Empty(t, evs)

	do(t, r)(r.RecordPayerDecision(PayerDecision{Outcome: OutcomeApproved, AuthNumber: "AUTH-1"}, t0.Add(time.Hour)))
	require.Equal(t, StateApproved, r.State)
	require.True(t, r.State.Terminal())
	require.Equal(t, "AUTH-1", r.Decision.AuthNumber)
	require.Equal(t, t0, r.DecidedAt)

	// Rebuild from envelopes yields the same aggregate.
	r2, evs2, err := Create("req-1", validIntake(), t0)
	require.NoError(t, err)
	var stream []Envelope
	envs, _ := r2.Emit(t0, "system", evs2)
	stream = append(stream, envs...)
	for _, step := range []func() ([]Event, error){
		func() ([]Event, error) { return r2.RecordDetermination(required(rules.Met), t0) },
		func() ([]Event, error) { return r2.RecordEvidence(completePackage(), t0) },
		func() ([]Event, error) { return r2.MarkSubmitted("claim-1", "", []byte(`{}`), t0) },
		func() ([]Event, error) {
			return r2.RecordPayerDecision(PayerDecision{Outcome: OutcomePended, PayerRef: "P-9"}, t0)
		},
		func() ([]Event, error) {
			return r2.RecordPayerDecision(PayerDecision{Outcome: OutcomeApproved, AuthNumber: "AUTH-1"}, t0.Add(time.Hour))
		},
	} {
		evs, err := step()
		require.NoError(t, err)
		envs, err := r2.Emit(t0, "test", evs)
		require.NoError(t, err)
		stream = append(stream, envs...)
	}
	loaded, err := Load(stream)
	require.NoError(t, err)
	require.Equal(t, StateApproved, loaded.State)
	require.Equal(t, r2.Version, loaded.Version)
	require.Equal(t, "AUTH-1", loaded.Decision.AuthNumber)
	for i, env := range stream {
		require.Equal(t, i+1, env.Seq, "sequence numbers are contiguous")
	}
}

func TestTransitionGuards(t *testing.T) {
	type step func(r *Request) ([]Event, error)
	det := func(d rules.Determination) step {
		return func(r *Request) ([]Event, error) { return r.RecordDetermination(d, t0) }
	}
	assemble := func(p Package) step {
		return func(r *Request) ([]Event, error) { return r.RecordEvidence(p, t0) }
	}
	submit := func(r *Request) ([]Event, error) { return r.MarkSubmitted("c", "", nil, t0) }
	payer := func(o PayerOutcome) step {
		return func(r *Request) ([]Event, error) { return r.RecordPayerDecision(PayerDecision{Outcome: o}, t0) }
	}
	review := func(rv Review) step {
		return func(r *Request) ([]Event, error) { return r.Review(rv, t0) }
	}

	tests := []struct {
		name  string
		setup []step
		cmd   step
		err   error
		msg   string
	}{
		{"assemble from draft", nil, assemble(completePackage()), ErrInvalidTransition, "cannot assemble in state draft"},
		{"assemble when not required", []step{det(rules.Determination{Decision: rules.NotRequired})}, assemble(completePackage()), ErrInvalidTransition, "not required"},
		{"submit from determined", []step{det(required(rules.Met))}, submit, ErrInvalidTransition, "cannot submit in state determined"},
		{"submit incomplete package", []step{det(required(rules.NotMet)), assemble(Package{Criteria: rules.NotMet})}, submit, ErrInvalidTransition, "incomplete"},
		{"determine after assembly", []step{det(required(rules.Met)), assemble(completePackage())}, det(required(rules.Met)), ErrInvalidTransition, "fixed once evidence"},
		{"payer decision before submit", []step{det(required(rules.Met))}, payer(OutcomeApproved), ErrInvalidTransition, ""},
		{"appeal when not denied", []step{det(required(rules.Met))}, review(Review{Action: ActionAppeal, Actor: "a", Reason: "x"}), ErrInvalidTransition, "only a denied"},
		{"appeal needs reason", []step{det(required(rules.Met)), assemble(completePackage()), submit, payer(OutcomeDenied)}, review(Review{Action: ActionAppeal, Actor: "a"}), ErrValidation, "reason"},
		{"review needs actor", nil, review(Review{Action: ActionWithdraw}), ErrValidation, "actor"},
		{"unknown action", nil, review(Review{Action: "dance", Actor: "a"}), ErrValidation, "unknown review action"},
		{"override needs reason", []step{det(required(rules.NotMet))}, review(Review{Action: ActionOverrideCriteria, Actor: "a", Reason: "short"}), ErrValidation, "reason"},
		{"override in wrong state", nil, review(Review{Action: ActionOverrideCriteria, Actor: "a", Reason: "long enough reason here"}), ErrInvalidTransition, ""},
		{"confirm without fact ids", []step{det(required(rules.Met)), assemble(completePackage())}, review(Review{Action: ActionConfirmEvidence, Actor: "a"}), ErrValidation, "fact_ids"},
		{"confirm without package", []step{det(required(rules.Met))}, review(Review{Action: ActionConfirmEvidence, Actor: "a", FactIDs: []string{"x"}}), ErrInvalidTransition, "no evidence"},
		{"confirm unknown fact", []step{det(required(rules.Met)), assemble(completePackage())}, review(Review{Action: ActionConfirmEvidence, Actor: "a", FactIDs: []string{"x"}}), ErrValidation, "not an extracted proposal"},
		{"set determination when not indeterminate", []step{det(required(rules.Met))}, review(Review{Action: ActionSetDetermination, Actor: "a", Decision: rules.Required}), ErrInvalidTransition, "indeterminate"},
		{"set determination bad decision", []step{det(rules.Determination{Decision: rules.Indeterminate})}, review(Review{Action: ActionSetDetermination, Actor: "a", Decision: "maybe"}), ErrValidation, "decision must be"},
		{"withdraw after approval", []step{det(required(rules.Met)), assemble(completePackage()), submit, payer(OutcomeApproved)}, review(Review{Action: ActionWithdraw, Actor: "a"}), ErrInvalidTransition, ""},
		{"review after close", []step{review(Review{Action: ActionWithdraw, Actor: "a", Reason: "dup"})}, review(Review{Action: ActionWithdraw, Actor: "a"}), ErrInvalidTransition, "closed"},
		{"submission failed guard", nil, func(r *Request) ([]Event, error) { return r.SubmissionFailed("boom", t0) }, ErrInvalidTransition, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRequest(t)
			for _, s := range tc.setup {
				do(t, r)(s(r))
			}
			_, err := tc.cmd(r)
			require.ErrorIs(t, err, tc.err)
			if tc.msg != "" {
				require.ErrorContains(t, err, tc.msg)
			}
		})
	}
}

func TestExceptionsLifecycle(t *testing.T) {
	r := newRequest(t)

	// Indeterminate → exception; human sets determination → resolved.
	do(t, r)(r.RecordDetermination(rules.Determination{Decision: rules.Indeterminate, RuleRef: "none", MissingData: []string{"no rule"}}, t0))
	require.Len(t, r.Exceptions, 1)
	require.Equal(t, ExcIndeterminate, r.Exceptions[0].Code)
	do(t, r)(r.Review(Review{Action: ActionSetDetermination, Actor: "um-nurse", Decision: rules.Required}, t0))
	require.Empty(t, r.Exceptions)
	require.Equal(t, rules.Required, r.Determination.Decision)
	require.Equal(t, "human:um-nurse", r.Determination.RuleRef)

	// Assembly with unknown criteria, a pending proposal and missing docs.
	proposal := evidence.Fact{ID: "llm-1", Kind: evidence.KindProcedure, Code: "97110", Prov: evidence.Provenance{Origin: evidence.OriginLLM}}
	pkg := Package{
		Criteria: rules.Unknown, MissingData: []string{"PT visits"}, Proposals: 1,
		Facts:         evidence.Set{proposal},
		Documentation: []DocStatus{{Code: "clinical_note", Satisfied: true}, {Code: "imaging_order"}, {Code: "optional_doc", Optional: true}},
	}
	do(t, r)(r.RecordEvidence(pkg, t0))
	codes := map[string]bool{}
	for _, x := range r.Exceptions {
		codes[x.Code] = true
	}
	require.Equal(t, map[string]bool{ExcCriteriaUnk: true, ExcEvidenceReview: true, ExcDocsMissing: true}, codes)

	// Raising the same code twice is a no-op.
	evs, err := r.RecordEvidence(pkg, t0)
	require.NoError(t, err)
	for _, e := range evs {
		_, isRaise := e.(ExceptionRaised)
		require.False(t, isRaise, "duplicate exception raised")
	}

	// Confirming the only proposal resolves the review exception.
	do(t, r)(r.Review(Review{Action: ActionConfirmEvidence, Actor: "md", FactIDs: []string{"llm-1"}}, t0))
	require.False(t, r.hasException(ExcEvidenceReview))
	require.Equal(t, "md", r.ConfirmedFacts["llm-1"])

	// Re-assembly with criteria met and docs present clears the rest.
	do(t, r)(r.RecordEvidence(completePackage(), t0))
	require.Empty(t, r.Exceptions)

	// Denial → exception → appeal resolves it and moves to Appealed.
	do(t, r)(r.MarkSubmitted("c1", "", nil, t0))
	do(t, r)(r.RecordPayerDecision(PayerDecision{Outcome: OutcomeDenied, Reasons: []string{"insufficient conservative therapy"}}, t0))
	require.Equal(t, StateDenied, r.State)
	require.True(t, r.hasException(ExcDenied))
	do(t, r)(r.Review(Review{Action: ActionAppeal, Actor: "md", Reason: "attached PT log"}, t0))
	require.Equal(t, StateAppealed, r.State)
	require.Empty(t, r.Exceptions)
	require.Equal(t, 1, r.Appeals)

	// Appeal submission keeps state Appealed and increments attempt.
	require.NoError(t, r.CanSubmit())
	do(t, r)(r.MarkSubmitted("c2", "", nil, t0))
	require.Equal(t, StateAppealed, r.State)
	require.True(t, r.Submission.Appeal)
	require.Equal(t, 2, r.Submission.Attempt)
	require.ErrorContains(t, r.CanSubmit(), "appeal already submitted")

	// SLA breach fires once, then approval clears SLA exceptions.
	require.Len(t, r.Breach(TimerResponse, t0, t0), 2)
	do(t, r)(r.Breach(TimerResponse, t0, t0), nil)
	require.Empty(t, r.Breach(TimerResponse, t0, t0))
	require.True(t, r.hasException(ExcSLAPrefix+"response"))
	do(t, r)(r.RecordPayerDecision(PayerDecision{Outcome: OutcomeApproved, AuthNumber: "A2"}, t0))
	require.Equal(t, StateApproved, r.State)
	require.Empty(t, r.Exceptions)
	require.Empty(t, r.Breach(TimerReview, t0, t0), "terminal state never breaches")
}

func TestOverrideMakesNotMetSubmittable(t *testing.T) {
	r := newRequest(t)
	do(t, r)(r.RecordDetermination(required(rules.NotMet), t0))
	do(t, r)(r.RecordEvidence(Package{Criteria: rules.NotMet, Documentation: []DocStatus{{Code: "clinical_note", Satisfied: true}}}, t0))
	require.True(t, r.hasException(ExcCriteriaNotMet))
	require.Error(t, r.CanSubmit())

	do(t, r)(r.Review(Review{Action: ActionOverrideCriteria, Actor: "dr.x", Reason: "red flags on exam not captured in codes"}, t0))
	require.False(t, r.hasException(ExcCriteriaNotMet))
	require.NotNil(t, r.Override)

	// The package is recomputed by the application via BuildPackage, which
	// honours the override.
	set, err := rules.NewSet([]rules.Rule{{ID: "x", Version: 1, Payer: "ACME_HEALTH", ServiceCodes: []string{"72148"}, RequiresAuth: true,
		Criteria:      &rules.Predicate{Fact: rules.FactDiagnosis, Codes: []string{"E11.*"}},
		Documentation: []rules.DocRequirement{{Code: "clinical_note"}, {Code: "extra", Optional: true}}}}, t0)
	require.NoError(t, err)
	pkg := r.BuildPackage(set, evidence.Set{{ID: "dx", Kind: evidence.KindDiagnosis, Code: "M54.5", Prov: evidence.Provenance{Origin: evidence.OriginStructured}}}, []string{"fhir"}, t0)
	require.Equal(t, rules.NotMet, pkg.Criteria)
	require.True(t, pkg.Complete, "override + docs → complete")
	require.Equal(t, "DocumentReference/1", pkg.Documentation[0].Source)
	do(t, r)(r.RecordEvidence(pkg, t0))
	require.NoError(t, r.CanSubmit())
}

func TestBuildPackage_ReviewsAndCorroboration(t *testing.T) {
	r := newRequest(t)
	r.Intake.Notes = []evidence.Note{{ID: "n1", Type: "imaging_order", Text: "MRI ordered"}}
	set, err := rules.NewSet([]rules.Rule{{ID: "x", Version: 1, Payer: "ACME_HEALTH", ServiceCodes: []string{"72148"}, RequiresAuth: true,
		Criteria:      &rules.Predicate{Fact: rules.FactProcedure, Codes: []string{"97110"}, MinCount: 2},
		Documentation: []rules.DocRequirement{{Code: "clinical_note"}, {Code: "imaging_order"}, {Code: "pt_log"}}}}, t0)
	require.NoError(t, err)

	structured := evidence.Fact{ID: "s1", Kind: evidence.KindProcedure, Code: "97110", Prov: evidence.Provenance{Origin: evidence.OriginStructured}}
	corroborated := evidence.Fact{ID: "l1", Kind: evidence.KindProcedure, Code: "97110", Prov: evidence.Provenance{Origin: evidence.OriginLLM}}
	pending := evidence.Fact{ID: "l2", Kind: evidence.KindProcedure, Code: "97140", Prov: evidence.Provenance{Origin: evidence.OriginLLM}}
	rejected := evidence.Fact{ID: "l3", Kind: evidence.KindDiagnosis, Code: "C73", Prov: evidence.Provenance{Origin: evidence.OriginLLM}}
	r.RejectedFacts["l3"] = "md"

	pkg := r.BuildPackage(set, evidence.Set{structured, corroborated, pending, rejected}, []string{"fhir", "llm"}, t0)
	require.Len(t, pkg.Facts, 3, "rejected proposal dropped")
	require.Equal(t, 1, pkg.Proposals, "only l2 awaits review")
	require.Equal(t, rules.Met, pkg.Criteria, "s1 + corroborated l1 count as 2")
	require.False(t, pkg.Complete, "pt_log missing")
	require.Equal(t, "note:n1", pkg.Documentation[1].Source)

	r.ConfirmedFacts["l2"] = "md"
	pkg = r.BuildPackage(set, evidence.Set{structured, corroborated, pending}, nil, t0)
	require.Equal(t, 0, pkg.Proposals)

	// No matching rule → unknown criteria, not a panic.
	empty, err := rules.NewSet([]rules.Rule{{ID: "y", Version: 1, Payer: "OTHER", ServiceCodes: []string{"1"}}}, t0)
	require.NoError(t, err)
	pkg = r.BuildPackage(empty, nil, nil, t0)
	require.Equal(t, rules.Unknown, pkg.Criteria)
	require.False(t, pkg.Complete)
}

func TestSLAPolicy(t *testing.T) {
	p := SLAPolicy{Determine: time.Hour, Assemble: 2 * time.Hour, Submit: 3 * time.Hour, Response: 4 * time.Hour, Pended: 72 * time.Hour, Review: 24 * time.Hour, UrgentFactor: 0.5}
	r := newRequest(t)

	d := p.Deadlines(r)
	require.Equal(t, []Deadline{{TimerDetermine, t0.Add(time.Hour)}}, d)
	require.Empty(t, p.Due(r, t0.Add(59*time.Minute)))
	require.Len(t, p.Due(r, t0.Add(time.Hour)), 1)

	r.Intake.Service.Urgent = true
	require.Equal(t, t0.Add(30*time.Minute), p.Deadlines(r)[0].DueAt, "urgent halves the deadline")
	r.Intake.Service.Urgent = false

	do(t, r)(r.RecordDetermination(rules.Determination{Decision: rules.NotRequired}, t0))
	require.Empty(t, p.Deadlines(r), "not required → nothing pending")

	r = newRequest(t)
	do(t, r)(r.RecordDetermination(rules.Determination{Decision: rules.Indeterminate}, t0))
	d = p.Deadlines(r)
	require.Len(t, d, 1)
	require.Equal(t, TimerReview, d[0].Timer, "open exception starts the review clock")

	r = newRequest(t)
	do(t, r)(r.RecordDetermination(required(rules.Met), t0))
	require.Equal(t, TimerAssemble, p.Deadlines(r)[0].Timer)
	do(t, r)(r.RecordEvidence(completePackage(), t0))
	require.Equal(t, TimerSubmit, p.Deadlines(r)[0].Timer)
	do(t, r)(r.MarkSubmitted("c", "", nil, t0))
	require.Equal(t, TimerResponse, p.Deadlines(r)[0].Timer)
	do(t, r)(r.RecordPayerDecision(PayerDecision{Outcome: OutcomePended, ReceivedAt: t0}, t0))
	require.Equal(t, []Deadline{{TimerPended, t0.Add(72 * time.Hour)}}, p.Deadlines(r))

	// Breached timers are excluded so they fire once.
	do(t, r)(r.Breach(TimerPended, t0.Add(72*time.Hour), t0.Add(73*time.Hour)), nil)
	for _, dl := range p.Deadlines(r) {
		require.NotEqual(t, TimerPended, dl.Timer)
	}
	require.True(t, r.hasException("sla_pended"))

	do(t, r)(r.RecordPayerDecision(PayerDecision{Outcome: OutcomeDenied}, t0))
	do(t, r)(r.Review(Review{Action: ActionAppeal, Actor: "a", Reason: "b"}, t0))
	require.Len(t, p.Deadlines(r), 1, "appealed without submission: only the review clock")
	do(t, r)(r.MarkSubmitted("c2", "", nil, t0))
	found := false
	for _, dl := range p.Deadlines(r) {
		found = found || dl.Timer == TimerResponse
	}
	require.True(t, found, "appeal submission restarts the response clock")

	zero := SLAPolicy{}
	require.Empty(t, zero.Deadlines(r), "zero policy disables timers")
}

func TestDecodeUnknownAndLoad(t *testing.T) {
	_, err := Decode(Envelope{Type: "nope", Payload: []byte(`{}`)})
	require.ErrorContains(t, err, "unknown event type")
	_, err = Decode(Envelope{Type: TypeClosed, Payload: []byte(`{`)})
	require.ErrorContains(t, err, "decode")
	_, err = Load(nil)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = Load([]Envelope{{Type: "nope", Payload: []byte(`{}`)}})
	require.Error(t, err)

	for _, ev := range []Event{RequestCreated{}, DeterminationRecorded{}, EvidenceAssembled{}, Submitted{}, PayerResponded{}, Reviewed{}, Appealed{}, ExceptionRaised{}, ExceptionResolved{}, SLABreached{}, Closed{}} {
		env, err := Wrap("r", 1, t0, "a", ev)
		require.NoError(t, err)
		back, err := Decode(env)
		require.NoError(t, err)
		require.Equal(t, ev.EventType(), back.EventType())
		require.IsType(t, ev, back, "decode returns value types")
	}
}
