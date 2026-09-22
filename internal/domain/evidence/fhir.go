package evidence

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// FHIRExtractorName identifies the deterministic structured extractor in
// provenance records. Bump the version when mapping semantics change.
const FHIRExtractorName = "fhir-r4@1"

// FHIRExtractor pulls facts from a FHIR R4 Bundle. It is deterministic: the
// same bundle always yields the same facts in the same order.
type FHIRExtractor struct{}

// Name implements Extractor.
func (FHIRExtractor) Name() string { return FHIRExtractorName }

// Minimal FHIR shapes. Only the fields the mapper needs are declared; the
// rest of each resource is ignored on purpose.
type (
	fhirBundle struct {
		ResourceType string `json:"resourceType"`
		Entry        []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	fhirCoding struct {
		System  string `json:"system"`
		Code    string `json:"code"`
		Display string `json:"display"`
	}
	fhirCodeable struct {
		Coding []fhirCoding `json:"coding"`
		Text   string       `json:"text"`
	}
	fhirQuantity struct {
		Value *float64 `json:"value"`
		Unit  string   `json:"unit"`
		Code  string   `json:"code"`
	}
	fhirPeriod struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}
	fhirResourceHeader struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	fhirPatient struct {
		fhirResourceHeader
		BirthDate string `json:"birthDate"`
		Gender    string `json:"gender"`
	}
	fhirCondition struct {
		fhirResourceHeader
		ClinicalStatus fhirCodeable `json:"clinicalStatus"`
		Code           fhirCodeable `json:"code"`
		OnsetDateTime  string       `json:"onsetDateTime"`
		RecordedDate   string       `json:"recordedDate"`
	}
	fhirObservation struct {
		fhirResourceHeader
		Status            string        `json:"status"`
		Code              fhirCodeable  `json:"code"`
		EffectiveDateTime string        `json:"effectiveDateTime"`
		Issued            string        `json:"issued"`
		ValueQuantity     *fhirQuantity `json:"valueQuantity"`
		ValueString       string        `json:"valueString"`
		ValueCodeable     *fhirCodeable `json:"valueCodeableConcept"`
	}
	fhirMedicationStatement struct {
		fhirResourceHeader
		Status                    string       `json:"status"`
		MedicationCodeableConcept fhirCodeable `json:"medicationCodeableConcept"`
		EffectiveDateTime         string       `json:"effectiveDateTime"`
		EffectivePeriod           *fhirPeriod  `json:"effectivePeriod"`
	}
	fhirProcedure struct {
		fhirResourceHeader
		Status            string       `json:"status"`
		Code              fhirCodeable `json:"code"`
		PerformedDateTime string       `json:"performedDateTime"`
		PerformedPeriod   *fhirPeriod  `json:"performedPeriod"`
	}
)

// Extract implements Extractor.
func (e FHIRExtractor) Extract(_ context.Context, in Input) (Set, error) {
	asOf := in.AsOf
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	var out Set
	birth := in.BirthDate
	sex := in.Sex

	if len(in.Bundle) > 0 {
		var b fhirBundle
		if err := json.Unmarshal(in.Bundle, &b); err != nil {
			return nil, fmt.Errorf("evidence: parse bundle: %w", err)
		}
		if b.ResourceType != "Bundle" {
			return nil, fmt.Errorf("evidence: expected resourceType Bundle, got %q", b.ResourceType)
		}
		for i, entry := range b.Entry {
			var hdr fhirResourceHeader
			if err := json.Unmarshal(entry.Resource, &hdr); err != nil {
				return nil, fmt.Errorf("evidence: entry %d: %w", i, err)
			}
			switch hdr.ResourceType {
			case "Patient":
				var p fhirPatient
				if err := json.Unmarshal(entry.Resource, &p); err != nil {
					return nil, fmt.Errorf("evidence: Patient %s: %w", hdr.ID, err)
				}
				if birth.IsZero() {
					birth = parseFHIRDate(p.BirthDate)
				}
				if sex == "" {
					sex = p.Gender
				}
			case "Condition":
				var c fhirCondition
				if err := json.Unmarshal(entry.Resource, &c); err != nil {
					return nil, fmt.Errorf("evidence: Condition %s: %w", hdr.ID, err)
				}
				out = append(out, e.condition(c)...)
			case "Observation":
				var o fhirObservation
				if err := json.Unmarshal(entry.Resource, &o); err != nil {
					return nil, fmt.Errorf("evidence: Observation %s: %w", hdr.ID, err)
				}
				out = append(out, e.observation(o)...)
			case "MedicationStatement":
				var m fhirMedicationStatement
				if err := json.Unmarshal(entry.Resource, &m); err != nil {
					return nil, fmt.Errorf("evidence: MedicationStatement %s: %w", hdr.ID, err)
				}
				out = append(out, e.medication(m)...)
			case "Procedure":
				var p fhirProcedure
				if err := json.Unmarshal(entry.Resource, &p); err != nil {
					return nil, fmt.Errorf("evidence: Procedure %s: %w", hdr.ID, err)
				}
				out = append(out, e.procedure(p)...)
			default:
				// Coverage, Organization, DocumentReference etc. carry no clinical facts.
			}
		}
	}

	if !birth.IsZero() {
		years := ageYears(birth, asOf)
		out = append(out, Fact{
			ID:        "demo-age",
			Kind:      KindDemographic,
			System:    SystemLocal,
			Code:      CodeAge,
			Display:   "age in years",
			Value:     &Quantity{Value: float64(years), Unit: "a"},
			Effective: asOf,
			Prov:      e.prov("Patient", ""),
		})
	}
	if sex != "" {
		out = append(out, Fact{
			ID:        "demo-sex",
			Kind:      KindDemographic,
			System:    SystemLocal,
			Code:      CodeSex,
			Text:      strings.ToLower(sex),
			Effective: asOf,
			Prov:      e.prov("Patient", ""),
		})
	}
	return out.Sorted(), nil
}

func (e FHIRExtractor) prov(rt, id string) Provenance {
	return Provenance{Origin: OriginStructured, ResourceType: rt, ResourceID: id, Extractor: FHIRExtractorName, Confidence: 1}
}

func (e FHIRExtractor) condition(c fhirCondition) []Fact {
	status := firstCode(c.ClinicalStatus)
	if status == "" {
		status = "active"
	}
	eff := parseFHIRDate(c.OnsetDateTime)
	if eff.IsZero() {
		eff = parseFHIRDate(c.RecordedDate)
	}
	var out []Fact
	for i, cd := range c.Code.Coding {
		if cd.Code == "" {
			continue
		}
		out = append(out, Fact{
			ID:        fmt.Sprintf("Condition/%s#%d", c.ID, i),
			Kind:      KindDiagnosis,
			System:    cd.System,
			Code:      cd.Code,
			Display:   displayOr(cd.Display, c.Code.Text),
			Status:    status,
			Effective: eff,
			Prov:      e.prov("Condition", c.ID),
		})
	}
	return out
}

func (e FHIRExtractor) observation(o fhirObservation) []Fact {
	eff := parseFHIRDate(o.EffectiveDateTime)
	if eff.IsZero() {
		eff = parseFHIRDate(o.Issued)
	}
	var out []Fact
	for i, cd := range o.Code.Coding {
		if cd.Code == "" {
			continue
		}
		f := Fact{
			ID:        fmt.Sprintf("Observation/%s#%d", o.ID, i),
			Kind:      KindObservation,
			System:    cd.System,
			Code:      cd.Code,
			Display:   displayOr(cd.Display, o.Code.Text),
			Status:    o.Status,
			Effective: eff,
			Prov:      e.prov("Observation", o.ID),
		}
		switch {
		case o.ValueQuantity != nil && o.ValueQuantity.Value != nil:
			unit := o.ValueQuantity.Code
			if unit == "" {
				unit = o.ValueQuantity.Unit
			}
			f.Value = &Quantity{Value: *o.ValueQuantity.Value, Unit: unit}
		case o.ValueCodeable != nil:
			f.Text = firstCode(*o.ValueCodeable)
			if f.Text == "" {
				f.Text = o.ValueCodeable.Text
			}
		case o.ValueString != "":
			f.Text = o.ValueString
		}
		out = append(out, f)
	}
	return out
}

func (e FHIRExtractor) medication(m fhirMedicationStatement) []Fact {
	eff := parseFHIRDate(m.EffectiveDateTime)
	if eff.IsZero() && m.EffectivePeriod != nil {
		eff = parseFHIRDate(m.EffectivePeriod.Start)
	}
	var out []Fact
	for i, cd := range m.MedicationCodeableConcept.Coding {
		if cd.Code == "" {
			continue
		}
		out = append(out, Fact{
			ID:        fmt.Sprintf("MedicationStatement/%s#%d", m.ID, i),
			Kind:      KindMedication,
			System:    cd.System,
			Code:      cd.Code,
			Display:   displayOr(cd.Display, m.MedicationCodeableConcept.Text),
			Status:    m.Status,
			Effective: eff,
			Prov:      e.prov("MedicationStatement", m.ID),
		})
	}
	return out
}

func (e FHIRExtractor) procedure(p fhirProcedure) []Fact {
	eff := parseFHIRDate(p.PerformedDateTime)
	if eff.IsZero() && p.PerformedPeriod != nil {
		eff = parseFHIRDate(p.PerformedPeriod.Start)
	}
	var out []Fact
	for i, cd := range p.Code.Coding {
		if cd.Code == "" {
			continue
		}
		out = append(out, Fact{
			ID:        fmt.Sprintf("Procedure/%s#%d", p.ID, i),
			Kind:      KindProcedure,
			System:    cd.System,
			Code:      cd.Code,
			Display:   displayOr(cd.Display, p.Code.Text),
			Status:    p.Status,
			Effective: eff,
			Prov:      e.prov("Procedure", p.ID),
		})
	}
	return out
}

func firstCode(c fhirCodeable) string {
	for _, cd := range c.Coding {
		if cd.Code != "" {
			return cd.Code
		}
	}
	return ""
}

func displayOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// parseFHIRDate accepts FHIR date/dateTime/instant precision variants.
func parseFHIRDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// ageYears computes completed years between birth and asOf.
func ageYears(birth, asOf time.Time) int {
	years := asOf.Year() - birth.Year()
	if asOf.Month() < birth.Month() || (asOf.Month() == birth.Month() && asOf.Day() < birth.Day()) {
		years--
	}
	if years < 0 {
		return 0
	}
	return years
}
