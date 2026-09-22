// Package pas builds and interprets Da Vinci PAS-shaped FHIR payloads: a
// request Bundle carrying a preauthorization Claim with its supporting
// resources, and the ClaimResponse that comes back.
//
// Only the subset of PAS the engine needs is modelled. X12 278 translation
// (which a real clearinghouse performs) is explicitly out of scope.
package pas

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
)

// Profile URLs (informational; the engine does not validate against them).
const (
	ProfileClaim         = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim"
	ProfileClaimResponse = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse"
	ExtReviewAction      = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction"
	ExtReviewActionCode  = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode"
	ExtItemAuthNumber    = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemAuthorizationNumber"
	ExtAuthorizedDate    = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemPreAuthPeriod"
	// X12 278 review action codes surfaced by PAS.
	ReviewApproved = "A1" // certified in total
	ReviewPartial  = "A2" // certified partial
	ReviewDenied   = "A3" // not certified
	ReviewPended   = "A4" // pended
	ReviewModified = "A6" // modified
	ReviewCancel   = "C"  // cancelled
	ReviewNoAction = "CT" // contact payer
)

// Map is a minimal ordered-enough JSON object. FHIR JSON has no ordering
// requirement, so a plain map keeps the builder honest and small.
type Map = map[string]any

// BuildRequest constructs the PAS request Bundle for a request. The Claim
// carries one item per requested service, the diagnoses, the provider,
// insurance and supportingInfo entries pointing at each usable fact, so a
// payer reviewer can see exactly what supported the request.
func BuildRequest(r *workflow.Request, claimID string, now time.Time) ([]byte, error) {
	if r == nil || r.Evidence == nil {
		return nil, fmt.Errorf("pas: request has no evidence package")
	}
	in := r.Intake
	patientRef := "Patient/" + in.Patient.ID
	providerRef := "Practitioner/" + in.Provider.NPI
	insurerRef := "Organization/" + in.Payer.ID
	coverageRef := "Coverage/" + in.Patient.MemberID

	var diagnosis []Map
	for i, dx := range in.Service.Diagnoses {
		diagnosis = append(diagnosis, Map{
			"sequence":                 i + 1,
			"diagnosisCodeableConcept": Map{"coding": []Map{{"system": evidence.SystemICD10, "code": dx}}},
			"type":                     []Map{{"coding": []Map{{"system": "http://terminology.hl7.org/CodeSystem/ex-diagnosistype", "code": ternary(i == 0, "principal", "secondary")}}}},
		})
	}
	dxSeq := make([]int, 0, len(in.Service.Diagnoses))
	for i := range in.Service.Diagnoses {
		dxSeq = append(dxSeq, i+1)
	}

	var supporting []Map
	seq := 1
	for _, f := range r.Evidence.Facts.Usable() {
		si := Map{
			"sequence": seq,
			"category": Map{"coding": []Map{{"system": "http://terminology.hl7.org/CodeSystem/claiminformationcategory", "code": "info"}}},
			"code":     Map{"coding": []Map{{"system": f.System, "code": f.Code, "display": f.Display}}},
		}
		if !f.Effective.IsZero() {
			si["timingDate"] = f.Effective.Format("2006-01-02")
		}
		if f.Value != nil {
			si["valueQuantity"] = Map{"value": f.Value.Value, "unit": f.Value.Unit}
		} else if f.Text != "" {
			si["valueString"] = f.Text
		}
		if f.Prov.ResourceType != "" {
			si["valueReference"] = Map{"reference": f.Prov.ResourceType + "/" + f.Prov.ResourceID}
		}
		supporting = append(supporting, si)
		seq++
	}
	for _, d := range r.Evidence.Documentation {
		if !d.Satisfied {
			continue
		}
		supporting = append(supporting, Map{
			"sequence":       seq,
			"category":       Map{"coding": []Map{{"system": "http://terminology.hl7.org/CodeSystem/claiminformationcategory", "code": "attachment"}}},
			"code":           Map{"coding": []Map{{"system": evidence.SystemLocal, "code": d.Code, "display": d.Display}}},
			"valueReference": Map{"reference": d.Source},
		})
		seq++
	}
	infoSeq := make([]int, 0, seq-1)
	for i := 1; i < seq; i++ {
		infoSeq = append(infoSeq, i)
	}

	claim := Map{
		"resourceType": "Claim",
		"id":           claimID,
		"meta":         Map{"profile": []string{ProfileClaim}},
		"identifier":   []Map{{"system": "urn:priorauth:request", "value": r.ID}},
		"status":       "active",
		"type":         Map{"coding": []Map{{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": ternary(in.Service.System == evidence.SystemRxNorm, "pharmacy", "professional")}}},
		"use":          "preauthorization",
		"patient":      Map{"reference": patientRef},
		"created":      now.UTC().Format(time.RFC3339),
		"insurer":      Map{"reference": insurerRef},
		"provider":     Map{"reference": providerRef},
		"priority":     Map{"coding": []Map{{"system": "http://terminology.hl7.org/CodeSystem/processpriority", "code": ternary(in.Service.Urgent, "stat", "normal")}}},
		"diagnosis":    diagnosis,
		"insurance":    []Map{{"sequence": 1, "focal": true, "coverage": Map{"reference": coverageRef}}},
		"item": []Map{{
			"sequence":            1,
			"diagnosisSequence":   dxSeq,
			"informationSequence": infoSeq,
			"productOrService":    Map{"coding": []Map{{"system": in.Service.System, "code": in.Service.Code, "display": in.Service.Description}}},
			"servicedDate":        in.Service.ServiceDate.Format("2006-01-02"),
			"quantity":            Map{"value": in.Service.Units},
		}},
	}
	if len(supporting) > 0 {
		claim["supportingInfo"] = supporting
	}
	if r.Determination != nil {
		claim["extension"] = []Map{{
			"url":         "urn:priorauth:determination",
			"valueString": fmt.Sprintf("%s criteria=%s ruleset=%s", r.Determination.RuleRef, r.Evidence.Criteria, r.Determination.RuleSetHash),
		}}
	}
	if r.Submission != nil && r.State == workflow.StateAppealed {
		claim["related"] = []Map{{
			"claim":        Map{"reference": "Claim/" + r.Submission.ClaimID},
			"relationship": Map{"coding": []Map{{"system": "http://terminology.hl7.org/CodeSystem/ex-relatedclaimrelationship", "code": "prior"}}},
		}}
		if r.Submission.PayerRef != "" {
			claim["related"].([]Map)[0]["reference"] = Map{"system": "urn:payer:preauth", "value": r.Submission.PayerRef}
		}
	}

	patient := Map{
		"resourceType": "Patient", "id": in.Patient.ID,
		"identifier": []Map{{"system": "urn:payer:member", "value": in.Patient.MemberID}},
		"birthDate":  in.Patient.BirthDate.Format("2006-01-02"),
	}
	if in.Patient.Name != "" {
		patient["name"] = []Map{{"text": in.Patient.Name}}
	}
	if in.Patient.Sex != "" {
		patient["gender"] = strings.ToLower(in.Patient.Sex)
	}
	coverage := Map{"resourceType": "Coverage", "id": in.Patient.MemberID, "status": "active",
		"subscriberId": in.Patient.MemberID, "beneficiary": Map{"reference": patientRef},
		"payor": []Map{{"reference": insurerRef}},
		"class": []Map{{"type": Map{"coding": []Map{{"system": "http://terminology.hl7.org/CodeSystem/coverage-class", "code": "plan"}}}, "value": in.Payer.Plan}}}
	practitioner := Map{"resourceType": "Practitioner", "id": in.Provider.NPI,
		"identifier": []Map{{"system": "http://hl7.org/fhir/sid/us-npi", "value": in.Provider.NPI}}}
	if in.Provider.Name != "" {
		practitioner["name"] = []Map{{"text": in.Provider.Name}}
	}
	insurer := Map{"resourceType": "Organization", "id": in.Payer.ID, "name": in.Payer.ID}

	entries := []Map{
		{"fullUrl": "urn:uuid:claim-" + claimID, "resource": claim},
		{"fullUrl": "urn:uuid:" + patientRef, "resource": patient},
		{"fullUrl": "urn:uuid:" + coverageRef, "resource": coverage},
		{"fullUrl": "urn:uuid:" + providerRef, "resource": practitioner},
		{"fullUrl": "urn:uuid:" + insurerRef, "resource": insurer},
	}
	bundle := Map{
		"resourceType": "Bundle",
		"id":           "pas-" + claimID,
		"type":         "collection",
		"timestamp":    now.UTC().Format(time.RFC3339),
		"entry":        entries,
	}
	b, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("pas: marshal bundle: %w", err)
	}
	return b, nil
}

// ClaimResponse is the subset of the PAS ClaimResponse the engine reads.
type ClaimResponse struct {
	ResourceType string `json:"resourceType"`
	ID           string `json:"id"`
	Status       string `json:"status"`
	Outcome      string `json:"outcome"` // queued | complete | error | partial
	Disposition  string `json:"disposition"`
	PreAuthRef   string `json:"preAuthRef"`
	Identifier   []struct {
		System string `json:"system"`
		Value  string `json:"value"`
	} `json:"identifier"`
	PreAuthPeriod *struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"preAuthPeriod"`
	Item []struct {
		ItemSequence int          `json:"itemSequence"`
		Extension    []extension  `json:"extension"`
		Adjudication []adjudicate `json:"adjudication"`
	} `json:"item"`
	Error []struct {
		Code struct {
			Coding []struct {
				Code    string `json:"code"`
				Display string `json:"display"`
			} `json:"coding"`
			Text string `json:"text"`
		} `json:"code"`
	} `json:"error"`
	ProcessNote []struct {
		Text string `json:"text"`
	} `json:"processNote"`
}

type extension struct {
	URL         string      `json:"url"`
	ValueString string      `json:"valueString,omitempty"`
	ValueCode   string      `json:"valueCode,omitempty"`
	ValueCoding *coding     `json:"valueCodeableConcept,omitempty"`
	Extension   []extension `json:"extension,omitempty"`
}

type coding struct {
	Coding []struct {
		Code string `json:"code"`
	} `json:"coding"`
}

type adjudicate struct {
	Category struct {
		Coding []struct {
			Code string `json:"code"`
		} `json:"coding"`
	} `json:"category"`
	Reason struct {
		Coding []struct {
			Code    string `json:"code"`
			Display string `json:"display"`
		} `json:"coding"`
		Text string `json:"text"`
	} `json:"reason"`
	Extension []extension `json:"extension"`
}

// Interpret maps a ClaimResponse onto a domain PayerDecision.
//
// Precedence: an explicit PAS reviewAction code on the item wins (A1/A2/A6
// approved, A3 denied, A4 pended); otherwise `outcome` decides (queued →
// pended, complete → approved, error → denied). Anything else is an error,
// because guessing a payer decision is worse than raising an exception.
func Interpret(raw []byte, receivedAt time.Time) (workflow.PayerDecision, error) {
	var cr ClaimResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return workflow.PayerDecision{}, fmt.Errorf("pas: parse ClaimResponse: %w", err)
	}
	if cr.ResourceType != "ClaimResponse" {
		return workflow.PayerDecision{}, fmt.Errorf("pas: expected ClaimResponse, got %q", cr.ResourceType)
	}
	// PayerRef is what we quote back to the payer on inquiry: the response
	// identifier (or id). AuthNumber is the certification the provider bills
	// with: preAuthRef, or the item-level PAS extension.
	d := workflow.PayerDecision{ReceivedAt: receivedAt, Raw: json.RawMessage(raw), AuthNumber: cr.PreAuthRef}
	for _, id := range cr.Identifier {
		if id.Value != "" {
			d.PayerRef = id.Value
			break
		}
	}
	if d.PayerRef == "" {
		d.PayerRef = cr.ID
	}
	if d.PayerRef == "" {
		d.PayerRef = cr.PreAuthRef
	}
	if cr.PreAuthPeriod != nil {
		d.ValidFrom, _ = time.Parse("2006-01-02", cr.PreAuthPeriod.Start)
		d.ValidTo, _ = time.Parse("2006-01-02", cr.PreAuthPeriod.End)
	}
	for _, e := range cr.Error {
		for _, c := range e.Code.Coding {
			d.Reasons = append(d.Reasons, strings.TrimSpace(c.Code+" "+c.Display))
		}
		if e.Code.Text != "" {
			d.Reasons = append(d.Reasons, e.Code.Text)
		}
	}
	for _, n := range cr.ProcessNote {
		if n.Text != "" {
			d.Reasons = append(d.Reasons, n.Text)
		}
	}

	if code := reviewAction(cr); code != "" {
		switch code {
		case ReviewApproved, ReviewPartial, ReviewModified:
			d.Outcome = workflow.OutcomeApproved
		case ReviewDenied, ReviewCancel:
			d.Outcome = workflow.OutcomeDenied
		case ReviewPended, ReviewNoAction:
			d.Outcome = workflow.OutcomePended
		default:
			return d, fmt.Errorf("pas: unknown reviewAction code %q", code)
		}
		if d.Outcome == workflow.OutcomeApproved && d.AuthNumber == "" {
			d.AuthNumber = itemAuthNumber(cr)
		}
		return d, nil
	}
	switch strings.ToLower(cr.Outcome) {
	case "queued":
		d.Outcome = workflow.OutcomePended
	case "complete", "partial":
		d.Outcome = workflow.OutcomeApproved
	case "error":
		d.Outcome = workflow.OutcomeDenied
		if len(d.Reasons) == 0 && cr.Disposition != "" {
			d.Reasons = []string{cr.Disposition}
		}
	default:
		return d, fmt.Errorf("pas: ClaimResponse has no reviewAction and outcome %q is not interpretable", cr.Outcome)
	}
	return d, nil
}

// reviewAction extracts the X12 review action code from item adjudication
// extensions, item extensions or the response-level disposition.
func reviewAction(cr ClaimResponse) string {
	var find func(exts []extension) string
	find = func(exts []extension) string {
		for _, e := range exts {
			if e.URL == ExtReviewActionCode {
				if e.ValueCode != "" {
					return e.ValueCode
				}
				if e.ValueCoding != nil && len(e.ValueCoding.Coding) > 0 {
					return e.ValueCoding.Coding[0].Code
				}
			}
			if c := find(e.Extension); c != "" {
				return c
			}
		}
		return ""
	}
	for _, it := range cr.Item {
		for _, adj := range it.Adjudication {
			if c := find(adj.Extension); c != "" {
				return c
			}
		}
		if c := find(it.Extension); c != "" {
			return c
		}
	}
	return ""
}

func itemAuthNumber(cr ClaimResponse) string {
	for _, it := range cr.Item {
		for _, e := range it.Extension {
			if e.URL == ExtItemAuthNumber && e.ValueString != "" {
				return e.ValueString
			}
		}
	}
	return ""
}

// BuildResponse constructs a PAS ClaimResponse. It is used by the payer
// simulator and by tests, and it is the inverse of Interpret.
func BuildResponse(id, claimID, patientRef string, outcome workflow.PayerOutcome, authNumber string, reasons []string, now time.Time) []byte {
	code, fhirOutcome := ReviewPended, "queued"
	switch outcome {
	case workflow.OutcomeApproved:
		code, fhirOutcome = ReviewApproved, "complete"
	case workflow.OutcomeDenied:
		code, fhirOutcome = ReviewDenied, "complete"
	}
	item := Map{
		"itemSequence": 1,
		"adjudication": []Map{{
			"category": Map{"coding": []Map{{"system": "http://terminology.hl7.org/CodeSystem/adjudication", "code": "submitted"}}},
			"extension": []Map{{
				"url": ExtReviewAction,
				"extension": []Map{{
					"url":                  ExtReviewActionCode,
					"valueCodeableConcept": Map{"coding": []Map{{"system": "https://codesystem.x12.org/005010/306", "code": code}}},
				}},
			}},
		}},
	}
	if authNumber != "" {
		item["extension"] = []Map{{"url": ExtItemAuthNumber, "valueString": authNumber}}
	}
	resp := Map{
		"resourceType": "ClaimResponse",
		"id":           id,
		"meta":         Map{"profile": []string{ProfileClaimResponse}},
		"identifier":   []Map{{"system": "urn:payer:preauth", "value": id}},
		"status":       "active",
		"type":         Map{"coding": []Map{{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": "professional"}}},
		"use":          "preauthorization",
		"patient":      Map{"reference": patientRef},
		"created":      now.UTC().Format(time.RFC3339),
		"insurer":      Map{"reference": "Organization/payer-sim"},
		"request":      Map{"reference": "Claim/" + claimID},
		"outcome":      fhirOutcome,
		"item":         []Map{item},
	}
	if authNumber != "" {
		resp["preAuthRef"] = authNumber
		resp["preAuthPeriod"] = Map{"start": now.Format("2006-01-02"), "end": now.AddDate(0, 3, 0).Format("2006-01-02")}
	}
	if len(reasons) > 0 {
		notes := make([]Map, 0, len(reasons))
		for i, r := range reasons {
			notes = append(notes, Map{"number": i + 1, "type": "display", "text": r})
		}
		resp["processNote"] = notes
		if outcome == workflow.OutcomeDenied {
			resp["disposition"] = strings.Join(reasons, "; ")
		}
	}
	b, _ := json.Marshal(resp) // static shape; cannot fail
	return b
}

func ternary[T any](c bool, a, b T) T {
	if c {
		return a
	}
	return b
}
