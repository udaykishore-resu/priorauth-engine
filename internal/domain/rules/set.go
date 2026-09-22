package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
)

// Set is an immutable, validated collection of rules with a content hash so
// callers can tell which rule set produced a decision.
type Set struct {
	Rules    []Rule    `json:"rules"`
	Hash     string    `json:"hash"`
	LoadedAt time.Time `json:"loaded_at"`
}

// NewSet validates rules, rejects duplicate id@version pairs and computes the
// content hash. Rules are sorted by (id, version) for deterministic output.
func NewSet(rules []Rule, loadedAt time.Time) (*Set, error) {
	var errs []error
	seen := map[string]bool{}
	sorted := make([]Rule, len(rules))
	copy(sorted, rules)
	for i := range sorted {
		if sorted[i].Plan == "" {
			sorted[i].Plan = Wildcard
		}
		if err := sorted[i].Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		ref := sorted[i].Ref()
		if seen[ref] {
			errs = append(errs, fmt.Errorf("duplicate rule %s", ref))
		}
		seen[ref] = true
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].ID != sorted[j].ID {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].Version < sorted[j].Version
	})
	h := sha256.New()
	for _, r := range sorted {
		b, _ := json.Marshal(r) // marshalling a validated struct cannot fail
		h.Write(b)
		h.Write([]byte{0})
	}
	return &Set{Rules: sorted, Hash: hex.EncodeToString(h.Sum(nil))[:16], LoadedAt: loadedAt}, nil
}

// LoadFS reads every *.json file under root (non-recursive) and builds a Set.
func LoadFS(fsys fs.FS, root string, loadedAt time.Time) (*Set, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, fmt.Errorf("rules: read dir %q: %w", root, err)
	}
	var rules []Rule
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(root, e.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("rules: read %s: %w", e.Name(), err))
			continue
		}
		r, err := ParseRule(data)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		rules = append(rules, r)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("rules: no rule files in %q", root)
	}
	return NewSet(rules, loadedAt)
}

// Request is what the selector needs to find the governing rule.
type Request struct {
	Payer          string
	Plan           string
	ServiceCode    string
	DiagnosisCodes []string
	ServiceDate    time.Time
}

// Select returns the most specific applicable rule, or ok=false.
//
// Specificity ordering (highest first): exact plan over wildcard, diagnosis
// scope present over absent, exact service code over pattern, exact payer over
// wildcard; ties are broken by highest version then lexical id so the result
// is total and deterministic.
func (s *Set) Select(req Request) (Rule, bool) {
	type scored struct {
		r     Rule
		score int
	}
	var best *scored
	for _, r := range s.Rules {
		sc, ok := score(r, req)
		if !ok {
			continue
		}
		if best == nil || sc > best.score ||
			(sc == best.score && (r.Version > best.r.Version || (r.Version == best.r.Version && r.ID < best.r.ID))) {
			best = &scored{r: r, score: sc}
		}
	}
	if best == nil {
		return Rule{}, false
	}
	return best.r, true
}

func score(r Rule, req Request) (int, bool) {
	sc := 0
	switch {
	case strings.EqualFold(r.Payer, req.Payer):
		sc += 1
	case r.Payer == Wildcard:
	default:
		return 0, false
	}
	switch {
	case strings.EqualFold(r.Plan, req.Plan):
		sc += 8
	case r.Plan == Wildcard:
	default:
		return 0, false
	}
	exact := false
	matchedSvc := false
	for _, c := range r.ServiceCodes {
		if matchCode(c, req.ServiceCode) {
			matchedSvc = true
			if !strings.HasSuffix(c, "*") {
				exact = true
			}
		}
	}
	if !matchedSvc {
		return 0, false
	}
	if exact {
		sc += 2
	}
	if len(r.DiagnosisScope) > 0 {
		hit := false
		for _, dx := range req.DiagnosisCodes {
			if matchAny(r.DiagnosisScope, dx) {
				hit = true
				break
			}
		}
		if !hit {
			return 0, false
		}
		sc += 4
	}
	if !req.ServiceDate.IsZero() {
		if !r.EffectiveFrom.IsZero() && req.ServiceDate.Before(r.EffectiveFrom) {
			return 0, false
		}
		if !r.EffectiveTo.IsZero() && !req.ServiceDate.Before(r.EffectiveTo) {
			return 0, false
		}
	}
	return sc, true
}

// Decision is the top-level outcome of a determination.
type Decision string

const (
	// NotRequired: the service does not need prior authorization.
	NotRequired Decision = "not_required"
	// Required: prior authorization is needed; see Criteria for support.
	Required Decision = "required"
	// Indeterminate: no rule covers the request; a human must decide.
	Indeterminate Decision = "indeterminate"
)

// Determination is the auditable output of evaluating a request.
type Determination struct {
	Decision      Decision         `json:"decision"`
	RuleRef       string           `json:"rule_ref"` // id@version or "none"
	RuleName      string           `json:"rule_name,omitempty"`
	RuleSetHash   string           `json:"rule_set_hash"`
	Criteria      Outcome          `json:"criteria,omitempty"` // only when Required
	Explain       *Node            `json:"explain,omitempty"`
	MissingData   []string         `json:"missing_data,omitempty"`
	Documentation []DocRequirement `json:"documentation,omitempty"`
	FactsUsed     int              `json:"facts_used"`
	EvaluatedAt   time.Time        `json:"evaluated_at"`
}

// Determine selects the governing rule and evaluates it.
func (s *Set) Determine(req Request, facts evidence.Set, asOf time.Time) Determination {
	d := Determination{RuleSetHash: s.Hash, EvaluatedAt: asOf, FactsUsed: len(facts.Usable())}
	r, ok := s.Select(req)
	if !ok {
		d.Decision, d.RuleRef = Indeterminate, "none"
		d.MissingData = []string{fmt.Sprintf("no coverage rule for payer=%s plan=%s service=%s", req.Payer, req.Plan, req.ServiceCode)}
		return d
	}
	d.RuleRef, d.RuleName = r.Ref(), r.Name
	if !r.RequiresAuth {
		d.Decision = NotRequired
		return d
	}
	d.Decision = Required
	tree := Evaluator{AsOf: asOf}.Evaluate(r.Criteria, facts)
	d.Explain = &tree
	d.Criteria = tree.Outcome
	d.MissingData = tree.MissingData()
	d.Documentation = r.Documentation
	return d
}
