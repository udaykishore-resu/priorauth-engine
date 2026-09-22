// Package evidence defines the clinical fact model that the rule evaluator
// consumes and the extractor contract that produces it.
//
// A Fact is a small, typed, provenance-carrying assertion about a patient
// ("has diagnosis M54.5", "BMI 33.1 kg/m2 on 2026-08-02", "6 PT visits").
// Facts never carry free text that the evaluator interprets; anything that
// came from a note is a Proposal until a human confirms it or structured
// data corroborates it.
package evidence

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Kind classifies a fact. The set is closed on purpose: rules reference kinds
// by name and a typo must fail rule validation, not silently never match.
type Kind string

const (
	KindDemographic Kind = "demographic" // age, sex
	KindDiagnosis   Kind = "diagnosis"   // ICD-10-CM
	KindMedication  Kind = "medication"  // RxNorm
	KindProcedure   Kind = "procedure"   // CPT / HCPCS / SNOMED
	KindObservation Kind = "observation" // LOINC + quantity
)

// ValidKind reports whether k is one of the closed set.
func ValidKind(k Kind) bool {
	switch k {
	case KindDemographic, KindDiagnosis, KindMedication, KindProcedure, KindObservation:
		return true
	}
	return false
}

// Well-known coding systems (FHIR canonical URLs).
const (
	SystemICD10  = "http://hl7.org/fhir/sid/icd-10-cm"
	SystemRxNorm = "http://www.nlm.nih.gov/research/umls/rxnorm"
	SystemCPT    = "http://www.ama-assn.org/go/cpt"
	SystemLOINC  = "http://loinc.org"
	SystemSNOMED = "http://snomed.info/sct"
	SystemLocal  = "urn:priorauth:local"
)

// Well-known demographic codes.
const (
	CodeAge = "age"
	CodeSex = "sex"
)

// Quantity is a numeric value with a unit. Units are UCUM-ish strings; the
// rules package owns conversion.
type Quantity struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
}

// Origin says where a fact came from. It is the audit trail for a decision.
type Origin string

const (
	OriginStructured Origin = "structured" // parsed from a FHIR resource
	OriginLLM        Origin = "llm"        // proposed by the bounded LLM extractor
	OriginHuman      Origin = "human"      // asserted or confirmed by a reviewer
)

// Provenance records how a fact was produced.
type Provenance struct {
	Origin       Origin  `json:"origin"`
	ResourceType string  `json:"resource_type,omitempty"`
	ResourceID   string  `json:"resource_id,omitempty"`
	Extractor    string  `json:"extractor"`              // name@version of the extractor
	Snippet      string  `json:"snippet,omitempty"`      // note excerpt (LLM proposals only)
	Confidence   float64 `json:"confidence,omitempty"`   // extractor self-reported, 0..1
	Corroborated bool    `json:"corroborated"`           // matched by a structured fact
	ConfirmedBy  string  `json:"confirmed_by,omitempty"` // reviewer id when human-confirmed
}

// Fact is one clinical assertion.
type Fact struct {
	ID        string     `json:"id"`
	Kind      Kind       `json:"kind"`
	System    string     `json:"system,omitempty"`
	Code      string     `json:"code"`
	Display   string     `json:"display,omitempty"`
	Status    string     `json:"status,omitempty"` // active|completed|stopped|final...
	Value     *Quantity  `json:"value,omitempty"`
	Text      string     `json:"text,omitempty"` // coded string values (e.g. sex=female)
	Effective time.Time  `json:"effective,omitempty"`
	Prov      Provenance `json:"provenance"`
}

// Usable reports whether the evaluator may rely on this fact. Structured and
// human facts are always usable; LLM proposals only once corroborated or
// confirmed. This is the single place that encodes the "no unreviewed LLM
// output drives a determination" invariant.
func (f Fact) Usable() bool {
	switch f.Prov.Origin {
	case OriginStructured, OriginHuman:
		return true
	case OriginLLM:
		return f.Prov.Corroborated || f.Prov.ConfirmedBy != ""
	}
	return false
}

// MatchKey is the identity used to corroborate an LLM proposal against a
// structured fact: same kind and code (system-insensitive, case-insensitive).
func (f Fact) MatchKey() string {
	return string(f.Kind) + "|" + strings.ToUpper(strings.TrimSpace(f.Code))
}

// String renders a compact human-readable form for explain trees and logs.
func (f Fact) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", f.Kind, f.Code)
	if f.Display != "" {
		fmt.Fprintf(&b, " (%s)", f.Display)
	}
	if f.Value != nil {
		fmt.Fprintf(&b, " = %g %s", f.Value.Value, f.Value.Unit)
	}
	if f.Text != "" {
		fmt.Fprintf(&b, " = %s", f.Text)
	}
	if !f.Effective.IsZero() {
		fmt.Fprintf(&b, " @ %s", f.Effective.Format("2006-01-02"))
	}
	return b.String()
}

// Set is an ordered collection of facts with helpers for the evaluator.
type Set []Fact

// Usable returns only facts the evaluator may rely on, in stable order.
func (s Set) Usable() Set {
	out := make(Set, 0, len(s))
	for _, f := range s {
		if f.Usable() {
			out = append(out, f)
		}
	}
	return out
}

// OfKind filters by kind.
func (s Set) OfKind(k Kind) Set {
	out := make(Set, 0, len(s))
	for _, f := range s {
		if f.Kind == k {
			out = append(out, f)
		}
	}
	return out
}

// Sorted returns a copy ordered by (kind, code, effective, id) so evaluation
// output is deterministic regardless of extractor iteration order.
func (s Set) Sorted() Set {
	out := make(Set, len(s))
	copy(out, s)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if !a.Effective.Equal(b.Effective) {
			return a.Effective.Before(b.Effective)
		}
		return a.ID < b.ID
	})
	return out
}

// Corroborate marks LLM proposals whose MatchKey matches a structured fact.
// It returns the updated set (a copy) and the number of corroborations.
func Corroborate(s Set) (Set, int) {
	structured := map[string]bool{}
	for _, f := range s {
		if f.Prov.Origin == OriginStructured {
			structured[f.MatchKey()] = true
		}
	}
	out := make(Set, len(s))
	copy(out, s)
	n := 0
	for i := range out {
		if out[i].Prov.Origin == OriginLLM && !out[i].Prov.Corroborated && structured[out[i].MatchKey()] {
			out[i].Prov.Corroborated = true
			n++
		}
	}
	return out, n
}

// Proposals returns LLM facts that are not yet usable (need human review).
func (s Set) Proposals() Set {
	out := Set{}
	for _, f := range s {
		if f.Prov.Origin == OriginLLM && !f.Usable() {
			out = append(out, f)
		}
	}
	return out
}
