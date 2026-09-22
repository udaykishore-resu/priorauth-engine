// Package rules implements the coverage-rules-as-code DSL: a small JSON
// format per payer/plan/service code that declares whether prior
// authorization is required, the clinical criteria that must be met, and the
// documentation that has to accompany a submission.
//
// Evaluation is deterministic, three-valued (met / not met / unknown) and
// always produces an explain tree so every determination is auditable down
// to the fact IDs that satisfied or failed each predicate.
package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Wildcard matches any payer/plan/code.
const Wildcard = "*"

// Rule is one coverage policy.
type Rule struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Name    string `json:"name"`
	// Payer is the payer identifier (e.g. "AETNA"). Wildcard allowed.
	Payer string `json:"payer"`
	// Plan is the plan/product identifier. Wildcard allowed (default).
	Plan string `json:"plan,omitempty"`
	// ServiceCodes are CPT/HCPCS codes (or prefix patterns like "9711*").
	ServiceCodes []string `json:"service_codes"`
	// DiagnosisScope optionally restricts the rule to requests carrying one
	// of these ICD-10 codes (prefix patterns allowed). A scoped rule is more
	// specific than an unscoped one and wins selection.
	DiagnosisScope []string `json:"diagnosis_scope,omitempty"`
	// EffectiveFrom / EffectiveTo bound when the rule applies (inclusive from,
	// exclusive to). Zero means unbounded.
	EffectiveFrom time.Time `json:"effective_from,omitempty"`
	EffectiveTo   time.Time `json:"effective_to,omitempty"`
	RequiresAuth  bool      `json:"requires_auth"`
	// Criteria must evaluate to Met for the auth to be clinically supported.
	// Nil means "no clinical criteria" (auth is administrative only).
	Criteria *Predicate `json:"criteria,omitempty"`
	// Documentation lists artefacts that must accompany the submission.
	Documentation []DocRequirement `json:"documentation,omitempty"`
	// Source records the policy document the rule encodes (for humans).
	Source string `json:"source,omitempty"`
}

// Ref renders the audit identifier "id@version".
func (r Rule) Ref() string { return fmt.Sprintf("%s@%d", r.ID, r.Version) }

// DocRequirement is a documentation artefact a payer expects.
type DocRequirement struct {
	// Code is a stable identifier matched against request attachment types
	// (e.g. "clinical_note", "imaging_order", "conservative_therapy_log").
	Code    string `json:"code"`
	Display string `json:"display"`
	// Optional marks documentation that is recommended but not blocking.
	Optional bool `json:"optional,omitempty"`
}

// Predicate is a node in the criteria tree. Exactly one of the combinator
// fields (All, Any, Not, AtLeast) or the leaf field Fact must be set.
type Predicate struct {
	// Description is shown in the explain tree; defaults to a generated one.
	Description string `json:"description,omitempty"`

	All     []Predicate `json:"all,omitempty"`
	Any     []Predicate `json:"any,omitempty"`
	Not     *Predicate  `json:"not,omitempty"`
	AtLeast *AtLeast    `json:"at_least,omitempty"`

	// Fact selects the leaf type: age | sex | diagnosis | medication |
	// procedure | observation.
	Fact string `json:"fact,omitempty"`
	// Codes / Code select facts by code; "M51.*" style prefix patterns allowed.
	Codes []string `json:"codes,omitempty"`
	Code  string   `json:"code,omitempty"`
	// Status restricts to facts with one of these statuses (default: any).
	Status []string `json:"status,omitempty"`
	// WithinDays restricts to facts effective within N days before AsOf.
	WithinDays int `json:"within_days,omitempty"`

	// Numeric comparison (age, observation). Op is one of >=, >, <=, <, ==, !=.
	Op    string   `json:"op,omitempty"`
	Value *float64 `json:"value,omitempty"`
	// Unit the Value is expressed in; observation values are converted.
	Unit string `json:"unit,omitempty"`
	// Aggregate picks which observation to compare when several match:
	// latest (default) | max | min.
	Aggregate string `json:"aggregate,omitempty"`

	// Equals compares a text fact (sex).
	Equals string `json:"equals,omitempty"`

	// MinCount requires at least N distinct matching facts (procedures,
	// diagnoses). Default 1.
	MinCount int `json:"min_count,omitempty"`
	// MinDurationDays requires a medication to have been effective for at
	// least N days before AsOf (prior therapy tried).
	MinDurationDays int `json:"min_duration_days,omitempty"`
}

// AtLeast requires N of the child predicates to be met.
type AtLeast struct {
	N  int         `json:"n"`
	Of []Predicate `json:"of"`
}

// Fact leaf types.
const (
	FactAge         = "age"
	FactSex         = "sex"
	FactDiagnosis   = "diagnosis"
	FactMedication  = "medication"
	FactProcedure   = "procedure"
	FactObservation = "observation"
)

var validOps = map[string]bool{">=": true, ">": true, "<=": true, "<": true, "==": true, "!=": true}

// Validate checks structural correctness so that a bad rule file is rejected
// at load time instead of silently never matching.
func (r Rule) Validate() error {
	var errs []error
	if strings.TrimSpace(r.ID) == "" {
		errs = append(errs, errors.New("id is required"))
	}
	if r.Version <= 0 {
		errs = append(errs, errors.New("version must be > 0"))
	}
	if strings.TrimSpace(r.Payer) == "" {
		errs = append(errs, errors.New("payer is required"))
	}
	if len(r.ServiceCodes) == 0 {
		errs = append(errs, errors.New("service_codes must not be empty"))
	}
	for _, c := range r.ServiceCodes {
		if strings.TrimSpace(c) == "" {
			errs = append(errs, errors.New("service_codes contains an empty code"))
		}
	}
	if !r.EffectiveFrom.IsZero() && !r.EffectiveTo.IsZero() && !r.EffectiveTo.After(r.EffectiveFrom) {
		errs = append(errs, errors.New("effective_to must be after effective_from"))
	}
	if r.Criteria != nil {
		if err := r.Criteria.validate("criteria"); err != nil {
			errs = append(errs, err)
		}
	}
	if r.RequiresAuth && r.Criteria == nil && len(r.Documentation) == 0 {
		errs = append(errs, errors.New("requires_auth rule must declare criteria or documentation"))
	}
	seen := map[string]bool{}
	for _, d := range r.Documentation {
		if d.Code == "" {
			errs = append(errs, errors.New("documentation entry without code"))
		}
		if seen[d.Code] {
			errs = append(errs, fmt.Errorf("documentation code %q duplicated", d.Code))
		}
		seen[d.Code] = true
	}
	if len(errs) > 0 {
		return fmt.Errorf("rule %q: %w", r.ID, errors.Join(errs...))
	}
	return nil
}

func (p *Predicate) validate(path string) error {
	set := 0
	if len(p.All) > 0 {
		set++
	}
	if len(p.Any) > 0 {
		set++
	}
	if p.Not != nil {
		set++
	}
	if p.AtLeast != nil {
		set++
	}
	if p.Fact != "" {
		set++
	}
	if set != 1 {
		return fmt.Errorf("%s: exactly one of all/any/not/at_least/fact must be set (got %d)", path, set)
	}
	var errs []error
	for i := range p.All {
		if err := p.All[i].validate(fmt.Sprintf("%s.all[%d]", path, i)); err != nil {
			errs = append(errs, err)
		}
	}
	for i := range p.Any {
		if err := p.Any[i].validate(fmt.Sprintf("%s.any[%d]", path, i)); err != nil {
			errs = append(errs, err)
		}
	}
	if p.Not != nil {
		if err := p.Not.validate(path + ".not"); err != nil {
			errs = append(errs, err)
		}
	}
	if p.AtLeast != nil {
		if p.AtLeast.N <= 0 || p.AtLeast.N > len(p.AtLeast.Of) {
			errs = append(errs, fmt.Errorf("%s.at_least: n must be within 1..len(of)", path))
		}
		for i := range p.AtLeast.Of {
			if err := p.AtLeast.Of[i].validate(fmt.Sprintf("%s.at_least.of[%d]", path, i)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if p.Fact != "" {
		switch p.Fact {
		case FactAge:
			if !validOps[p.Op] || p.Value == nil {
				errs = append(errs, fmt.Errorf("%s: age requires op and value", path))
			}
		case FactSex:
			if p.Equals == "" {
				errs = append(errs, fmt.Errorf("%s: sex requires equals", path))
			}
		case FactDiagnosis, FactProcedure, FactMedication:
			if len(p.Codes) == 0 && p.Code == "" {
				errs = append(errs, fmt.Errorf("%s: %s requires codes", path, p.Fact))
			}
			if p.Fact != FactMedication && p.MinDurationDays != 0 {
				errs = append(errs, fmt.Errorf("%s: min_duration_days only valid for medication", path))
			}
		case FactObservation:
			if len(p.Codes) == 0 && p.Code == "" {
				errs = append(errs, fmt.Errorf("%s: observation requires code", path))
			}
			if p.Op != "" && (!validOps[p.Op] || p.Value == nil) {
				errs = append(errs, fmt.Errorf("%s: observation op requires a valid op and value", path))
			}
			if p.Aggregate != "" && p.Aggregate != "latest" && p.Aggregate != "max" && p.Aggregate != "min" {
				errs = append(errs, fmt.Errorf("%s: aggregate must be latest|max|min", path))
			}
		default:
			errs = append(errs, fmt.Errorf("%s: unknown fact type %q", path, p.Fact))
		}
		if p.Op != "" && !validOps[p.Op] {
			errs = append(errs, fmt.Errorf("%s: invalid op %q", path, p.Op))
		}
		if p.WithinDays < 0 || p.MinCount < 0 || p.MinDurationDays < 0 {
			errs = append(errs, fmt.Errorf("%s: negative window/count", path))
		}
	}
	return errors.Join(errs...)
}

// codes returns the union of Code and Codes.
func (p *Predicate) codes() []string {
	if p.Code != "" {
		return append([]string{p.Code}, p.Codes...)
	}
	return p.Codes
}

// ParseRule decodes and validates one rule document.
func ParseRule(data []byte) (Rule, error) {
	var r Rule
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Rule{}, fmt.Errorf("rules: decode: %w", err)
	}
	if r.Plan == "" {
		r.Plan = Wildcard
	}
	if err := r.Validate(); err != nil {
		return Rule{}, err
	}
	return r, nil
}

// matchCode reports whether code matches pattern. Patterns are exact or a
// prefix followed by "*" ("M51.*"). Comparison is case-insensitive.
func matchCode(pattern, code string) bool {
	pattern = strings.ToUpper(strings.TrimSpace(pattern))
	code = strings.ToUpper(strings.TrimSpace(code))
	if pattern == Wildcard {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(code, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == code
}

func matchAny(patterns []string, code string) bool {
	for _, p := range patterns {
		if matchCode(p, code) {
			return true
		}
	}
	return false
}
