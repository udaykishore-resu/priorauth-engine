package pas

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/rules"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
)

var now = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func sampleRequest(t *testing.T) *workflow.Request {
	t.Helper()
	in := workflow.Intake{
		Patient:     workflow.Patient{ID: "pat-1", MemberID: "M1", Name: "Jane Doe", BirthDate: time.Date(1971, 5, 14, 0, 0, 0, 0, time.UTC), Sex: "Female"},
		Payer:       workflow.Payer{ID: "ACME_HEALTH", Plan: "HMO"},
		Provider:    workflow.Provider{NPI: "1234567893", Name: "Dr. Spine"},
		Service:     workflow.Service{Code: "72148", Description: "MRI lumbar", Diagnoses: []string{"M54.16", "M51.16"}, ServiceDate: now, Urgent: true},
		Attachments: []workflow.Attachment{{Type: "clinical_note", Ref: "DocumentReference/1"}},
	}
	r, evs, err := workflow.Create("req-1", in, now)
	require.NoError(t, err)
	_, err = r.Emit(now, "t", evs)
	require.NoError(t, err)
	det := rules.Determination{Decision: rules.Required, RuleRef: "rule@1", RuleSetHash: "abc", Criteria: rules.Met}
	evs, err = r.RecordDetermination(det, now)
	require.NoError(t, err)
	_, err = r.Emit(now, "t", evs)
	require.NoError(t, err)
	pkg := workflow.Package{
		Criteria: rules.Met, Complete: true,
		Facts: evidence.Set{
			{ID: "dx", Kind: evidence.KindDiagnosis, System: evidence.SystemICD10, Code: "M54.16", Effective: now, Prov: evidence.Provenance{Origin: evidence.OriginStructured, ResourceType: "Condition", ResourceID: "c1"}},
			{ID: "bmi", Kind: evidence.KindObservation, System: evidence.SystemLOINC, Code: "39156-5", Value: &evidence.Quantity{Value: 28.4, Unit: "kg/m2"}, Prov: evidence.Provenance{Origin: evidence.OriginStructured}},
			{ID: "sex", Kind: evidence.KindDemographic, Code: "sex", Text: "female", Prov: evidence.Provenance{Origin: evidence.OriginStructured}},
			{ID: "llm", Kind: evidence.KindProcedure, Code: "97110", Prov: evidence.Provenance{Origin: evidence.OriginLLM}},
		},
		Documentation: []workflow.DocStatus{{Code: "clinical_note", Satisfied: true, Source: "DocumentReference/1"}, {Code: "missing"}},
	}
	evs, err = r.RecordEvidence(pkg, now)
	require.NoError(t, err)
	_, err = r.Emit(now, "t", evs)
	require.NoError(t, err)
	return r
}

func TestBuildRequest(t *testing.T) {
	r := sampleRequest(t)
	b, err := BuildRequest(r, "claim-1", now)
	require.NoError(t, err)

	var bundle struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
		Entry        []struct {
			Resource map[string]any `json:"resource"`
		} `json:"entry"`
	}
	require.NoError(t, json.Unmarshal(b, &bundle))
	require.Equal(t, "Bundle", bundle.ResourceType)
	require.Equal(t, "collection", bundle.Type)
	require.Len(t, bundle.Entry, 5)

	claim := bundle.Entry[0].Resource
	require.Equal(t, "Claim", claim["resourceType"])
	require.Equal(t, "preauthorization", claim["use"])
	require.Equal(t, "Patient/pat-1", claim["patient"].(map[string]any)["reference"])
	require.Len(t, claim["diagnosis"], 2)
	// 3 usable facts (LLM proposal excluded) + 1 satisfied attachment.
	require.Len(t, claim["supportingInfo"], 4)
	item := claim["item"].([]any)[0].(map[string]any)
	require.Equal(t, "72148", item["productOrService"].(map[string]any)["coding"].([]any)[0].(map[string]any)["code"])
	require.Len(t, item["informationSequence"], 4)
	require.Equal(t, "stat", claim["priority"].(map[string]any)["coding"].([]any)[0].(map[string]any)["code"])
	require.Contains(t, claim["extension"].([]any)[0].(map[string]any)["valueString"], "rule@1")
	require.Nil(t, claim["related"])

	patient := bundle.Entry[1].Resource
	require.Equal(t, "female", patient["gender"])
	require.Equal(t, "1971-05-14", patient["birthDate"])

	// Deterministic output.
	b2, err := BuildRequest(r, "claim-1", now)
	require.NoError(t, err)
	require.JSONEq(t, string(b), string(b2))
}

func TestBuildRequest_AppealAndErrors(t *testing.T) {
	_, err := BuildRequest(nil, "c", now)
	require.Error(t, err)
	_, err = BuildRequest(&workflow.Request{}, "c", now)
	require.Error(t, err)

	r := sampleRequest(t)
	for _, step := range []func() ([]workflow.Event, error){
		func() ([]workflow.Event, error) { return r.MarkSubmitted("claim-1", "", nil, now) },
		func() ([]workflow.Event, error) {
			return r.RecordPayerDecision(workflow.PayerDecision{Outcome: workflow.OutcomeDenied, PayerRef: "P-1"}, now)
		},
		func() ([]workflow.Event, error) {
			return r.Review(workflow.Review{Action: workflow.ActionAppeal, Actor: "md", Reason: "more evidence"}, now)
		},
	} {
		evs, err := step()
		require.NoError(t, err)
		_, err = r.Emit(now, "t", evs)
		require.NoError(t, err)
	}
	b, err := BuildRequest(r, "claim-2", now)
	require.NoError(t, err)
	var bundle struct {
		Entry []struct {
			Resource struct {
				Related []struct {
					Claim     map[string]string `json:"claim"`
					Reference map[string]string `json:"reference"`
				} `json:"related"`
			} `json:"resource"`
		} `json:"entry"`
	}
	require.NoError(t, json.Unmarshal(b, &bundle))
	require.Equal(t, "Claim/claim-1", bundle.Entry[0].Resource.Related[0].Claim["reference"])
	require.Equal(t, "P-1", bundle.Entry[0].Resource.Related[0].Reference["value"])
}

func TestInterpret(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    workflow.PayerOutcome
		auth    string
		reasons []string
		err     string
	}{
		{"approved via builder", string(BuildResponse("P1", "c", "Patient/p", workflow.OutcomeApproved, "AUTH-9", nil, now)), workflow.OutcomeApproved, "AUTH-9", nil, ""},
		{"denied via builder", string(BuildResponse("P2", "c", "Patient/p", workflow.OutcomeDenied, "", []string{"no PT"}, now)), workflow.OutcomeDenied, "", []string{"no PT"}, ""},
		{"pended via builder", string(BuildResponse("P3", "c", "Patient/p", workflow.OutcomePended, "", nil, now)), workflow.OutcomePended, "", nil, ""},
		{"partial code A2 → approved", `{"resourceType":"ClaimResponse","id":"x","outcome":"complete","item":[{"itemSequence":1,"extension":[{"url":"` + ExtReviewActionCode + `","valueCode":"A2"},{"url":"` + ExtItemAuthNumber + `","valueString":"A-77"}]}]}`, workflow.OutcomeApproved, "A-77", nil, ""},
		{"cancelled C → denied", `{"resourceType":"ClaimResponse","id":"x","outcome":"complete","item":[{"itemSequence":1,"extension":[{"url":"` + ExtReviewActionCode + `","valueCode":"C"}]}]}`, workflow.OutcomeDenied, "", nil, ""},
		{"CT → pended", `{"resourceType":"ClaimResponse","id":"x","outcome":"complete","item":[{"itemSequence":1,"extension":[{"url":"` + ExtReviewActionCode + `","valueCode":"CT"}]}]}`, workflow.OutcomePended, "", nil, ""},
		{"unknown code", `{"resourceType":"ClaimResponse","id":"x","outcome":"complete","item":[{"itemSequence":1,"extension":[{"url":"` + ExtReviewActionCode + `","valueCode":"ZZ"}]}]}`, "", "", nil, "unknown reviewAction"},
		{"fallback queued", `{"resourceType":"ClaimResponse","id":"x","outcome":"queued"}`, workflow.OutcomePended, "", nil, ""},
		{"fallback complete", `{"resourceType":"ClaimResponse","id":"x","outcome":"complete","preAuthRef":"R1","preAuthPeriod":{"start":"2026-09-01","end":"2026-12-01"}}`, workflow.OutcomeApproved, "R1", nil, ""},
		{"fallback error with disposition", `{"resourceType":"ClaimResponse","id":"x","outcome":"error","disposition":"member not found"}`, workflow.OutcomeDenied, "", []string{"member not found"}, ""},
		{"error with coded reasons", `{"resourceType":"ClaimResponse","id":"x","outcome":"error","error":[{"code":{"coding":[{"code":"a001","display":"Missing"}],"text":"txt"}}]}`, workflow.OutcomeDenied, "", []string{"a001 Missing", "txt"}, ""},
		{"uninterpretable", `{"resourceType":"ClaimResponse","id":"x","outcome":"weird"}`, "", "", nil, "not interpretable"},
		{"wrong resource", `{"resourceType":"OperationOutcome"}`, "", "", nil, "expected ClaimResponse"},
		{"bad json", `{`, "", "", nil, "parse"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Interpret([]byte(tc.raw), now)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, d.Outcome)
			require.Equal(t, tc.auth, d.AuthNumber)
			require.Equal(t, tc.reasons, d.Reasons)
			require.NotEmpty(t, d.PayerRef)
			require.Equal(t, now, d.ReceivedAt)
			require.JSONEq(t, tc.raw, string(d.Raw))
		})
	}
	t.Run("valid period parsed", func(t *testing.T) {
		d, err := Interpret([]byte(`{"resourceType":"ClaimResponse","id":"x","outcome":"complete","preAuthRef":"R1","preAuthPeriod":{"start":"2026-09-01","end":"2026-12-01"}}`), now)
		require.NoError(t, err)
		require.Equal(t, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), d.ValidTo)
	})
}

func FuzzInterpret(f *testing.F) {
	f.Add(BuildResponse("P1", "c", "Patient/p", workflow.OutcomeApproved, "AUTH", []string{"ok"}, now))
	f.Add([]byte(`{"resourceType":"ClaimResponse","outcome":"queued"}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := Interpret(data, now)
		if err == nil {
			require.Contains(t, []workflow.PayerOutcome{workflow.OutcomeApproved, workflow.OutcomeDenied, workflow.OutcomePended}, d.Outcome)
		}
	})
}
