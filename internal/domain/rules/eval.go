package rules

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
)

// Outcome is the three-valued result of a predicate.
type Outcome string

const (
	Met     Outcome = "met"
	NotMet  Outcome = "not_met"
	Unknown Outcome = "unknown" // insufficient data to decide
)

// Node is one entry of the explain tree.
type Node struct {
	Description string   `json:"description"`
	Outcome     Outcome  `json:"outcome"`
	Reason      string   `json:"reason,omitempty"`
	Evidence    []string `json:"evidence,omitempty"` // fact IDs that decided the node
	Children    []Node   `json:"children,omitempty"`
}

// MissingData returns the descriptions of the Unknown leaves that keep the
// tree from resolving: it only descends through Unknown nodes, so a tree
// that is already Met or NotMet reports nothing to chase.
func (n Node) MissingData() []string {
	var out []string
	var walk func(Node)
	walk = func(x Node) {
		if x.Outcome != Unknown {
			return
		}
		if len(x.Children) == 0 {
			out = append(out, x.Description)
			return
		}
		for _, c := range x.Children {
			walk(c)
		}
	}
	walk(n)
	return out
}

// Evaluator evaluates predicates against a fact set at a point in time.
type Evaluator struct {
	AsOf time.Time
}

// Evaluate returns the explain tree for p over facts. Only usable facts are
// considered (see evidence.Fact.Usable).
func (e Evaluator) Evaluate(p *Predicate, facts evidence.Set) Node {
	if p == nil {
		return Node{Description: "no clinical criteria", Outcome: Met}
	}
	usable := facts.Usable().Sorted()
	return e.eval(p, usable)
}

func (e Evaluator) eval(p *Predicate, facts evidence.Set) Node {
	switch {
	case len(p.All) > 0:
		n := Node{Description: descOr(p.Description, "all of")}
		for i := range p.All {
			n.Children = append(n.Children, e.eval(&p.All[i], facts))
		}
		n.Outcome = combineAll(n.Children)
		return n
	case len(p.Any) > 0:
		n := Node{Description: descOr(p.Description, "any of")}
		for i := range p.Any {
			n.Children = append(n.Children, e.eval(&p.Any[i], facts))
		}
		n.Outcome = combineAny(n.Children)
		return n
	case p.Not != nil:
		child := e.eval(p.Not, facts)
		n := Node{Description: descOr(p.Description, "not"), Children: []Node{child}}
		switch child.Outcome {
		case Met:
			n.Outcome = NotMet
		case NotMet:
			n.Outcome = Met
		default:
			n.Outcome = Unknown
		}
		return n
	case p.AtLeast != nil:
		n := Node{Description: descOr(p.Description, fmt.Sprintf("at least %d of", p.AtLeast.N))}
		met, unknown := 0, 0
		for i := range p.AtLeast.Of {
			c := e.eval(&p.AtLeast.Of[i], facts)
			n.Children = append(n.Children, c)
			switch c.Outcome {
			case Met:
				met++
			case Unknown:
				unknown++
			}
		}
		switch {
		case met >= p.AtLeast.N:
			n.Outcome = Met
		case met+unknown >= p.AtLeast.N:
			n.Outcome = Unknown
		default:
			n.Outcome = NotMet
		}
		n.Reason = fmt.Sprintf("%d met, %d unknown", met, unknown)
		return n
	}
	return e.leaf(p, facts)
}

func combineAll(children []Node) Outcome {
	out := Met
	for _, c := range children {
		switch c.Outcome {
		case NotMet:
			return NotMet
		case Unknown:
			out = Unknown
		}
	}
	return out
}

func combineAny(children []Node) Outcome {
	out := NotMet
	for _, c := range children {
		switch c.Outcome {
		case Met:
			return Met
		case Unknown:
			out = Unknown
		}
	}
	return out
}

func (e Evaluator) leaf(p *Predicate, facts evidence.Set) Node {
	switch p.Fact {
	case FactAge:
		return e.age(p, facts)
	case FactSex:
		return e.sex(p, facts)
	case FactDiagnosis:
		return e.coded(p, facts, evidence.KindDiagnosis)
	case FactProcedure:
		return e.coded(p, facts, evidence.KindProcedure)
	case FactMedication:
		return e.medication(p, facts)
	case FactObservation:
		return e.observation(p, facts)
	}
	return Node{Description: p.Fact, Outcome: Unknown, Reason: "unsupported fact type"}
}

func (e Evaluator) age(p *Predicate, facts evidence.Set) Node {
	n := Node{Description: descOr(p.Description, fmt.Sprintf("age %s %g", p.Op, *p.Value))}
	for _, f := range facts.OfKind(evidence.KindDemographic) {
		if f.Code == evidence.CodeAge && f.Value != nil {
			n.Evidence = []string{f.ID}
			if compare(f.Value.Value, p.Op, *p.Value) {
				n.Outcome = Met
			} else {
				n.Outcome = NotMet
			}
			n.Reason = fmt.Sprintf("age is %g", f.Value.Value)
			return n
		}
	}
	n.Outcome, n.Reason = Unknown, "no birth date on file"
	return n
}

func (e Evaluator) sex(p *Predicate, facts evidence.Set) Node {
	n := Node{Description: descOr(p.Description, "sex is "+p.Equals)}
	for _, f := range facts.OfKind(evidence.KindDemographic) {
		if f.Code == evidence.CodeSex {
			n.Evidence = []string{f.ID}
			if strings.EqualFold(f.Text, p.Equals) {
				n.Outcome = Met
			} else {
				n.Outcome = NotMet
			}
			n.Reason = "sex is " + f.Text
			return n
		}
	}
	n.Outcome, n.Reason = Unknown, "sex not recorded"
	return n
}

// coded handles diagnosis and procedure leaves: code match, optional status
// and recency filters, and a distinct-occurrence count.
func (e Evaluator) coded(p *Predicate, facts evidence.Set, kind evidence.Kind) Node {
	minCount := p.MinCount
	if minCount <= 0 {
		minCount = 1
	}
	n := Node{Description: descOr(p.Description, fmt.Sprintf("%s in %s%s", kind, strings.Join(p.codes(), ","), windowDesc(p, minCount)))}
	all := facts.OfKind(kind)
	if len(all) == 0 {
		n.Outcome, n.Reason = Unknown, fmt.Sprintf("no %s facts on file", kind)
		return n
	}
	matched := e.filter(p, all)
	n.Evidence = ids(matched)
	if len(matched) >= minCount {
		n.Outcome = Met
		n.Reason = fmt.Sprintf("%d matching", len(matched))
	} else {
		n.Outcome = NotMet
		n.Reason = fmt.Sprintf("%d matching, need %d", len(matched), minCount)
	}
	return n
}

func (e Evaluator) medication(p *Predicate, facts evidence.Set) Node {
	n := Node{Description: descOr(p.Description, fmt.Sprintf("medication in %s%s", strings.Join(p.codes(), ","), durationDesc(p)))}
	all := facts.OfKind(evidence.KindMedication)
	if len(all) == 0 {
		n.Outcome, n.Reason = Unknown, "no medication history on file"
		return n
	}
	matched := e.filter(p, all)
	if len(matched) == 0 {
		n.Outcome, n.Reason = NotMet, "no matching medication"
		return n
	}
	n.Evidence = ids(matched)
	if p.MinDurationDays <= 0 {
		n.Outcome, n.Reason = Met, fmt.Sprintf("%d matching", len(matched))
		return n
	}
	// Longest single course satisfies the duration requirement.
	best, undated := 0.0, 0
	for _, f := range matched {
		if f.Effective.IsZero() {
			undated++
			continue
		}
		d := e.asOf().Sub(f.Effective).Hours() / 24
		best = math.Max(best, d)
	}
	switch {
	case best >= float64(p.MinDurationDays):
		n.Outcome, n.Reason = Met, fmt.Sprintf("on therapy %.0f days", best)
	case undated > 0:
		n.Outcome, n.Reason = Unknown, "medication start date missing"
	default:
		n.Outcome, n.Reason = NotMet, fmt.Sprintf("on therapy %.0f days, need %d", best, p.MinDurationDays)
	}
	return n
}

func (e Evaluator) observation(p *Predicate, facts evidence.Set) Node {
	desc := fmt.Sprintf("observation %s", strings.Join(p.codes(), ","))
	if p.Op != "" {
		desc += fmt.Sprintf(" %s %g %s", p.Op, *p.Value, p.Unit)
	}
	n := Node{Description: descOr(p.Description, desc)}
	matched := e.filter(p, facts.OfKind(evidence.KindObservation))
	if len(matched) == 0 {
		n.Outcome, n.Reason = Unknown, "observation not on file"
		return n
	}
	if p.Op == "" {
		n.Evidence, n.Outcome, n.Reason = ids(matched), Met, "present"
		return n
	}
	// Convert every candidate; drop non-numeric / unconvertible ones.
	type cand struct {
		f evidence.Fact
		v float64
	}
	var cands []cand
	var convErr error
	for _, f := range matched {
		if f.Value == nil {
			continue
		}
		v, err := convert(f.Code, f.Value.Value, f.Value.Unit, p.Unit)
		if err != nil {
			convErr = err
			continue
		}
		cands = append(cands, cand{f, v})
	}
	if len(cands) == 0 {
		n.Outcome = Unknown
		if convErr != nil {
			n.Reason = convErr.Error()
		} else {
			n.Reason = "observation has no numeric value"
		}
		return n
	}
	pick := cands[len(cands)-1] // Sorted() orders by effective asc → latest last
	switch p.Aggregate {
	case "max":
		for _, c := range cands {
			if c.v > pick.v {
				pick = c
			}
		}
	case "min":
		for _, c := range cands {
			if c.v < pick.v {
				pick = c
			}
		}
	}
	n.Evidence = []string{pick.f.ID}
	if compare(pick.v, p.Op, *p.Value) {
		n.Outcome = Met
	} else {
		n.Outcome = NotMet
	}
	n.Reason = fmt.Sprintf("value %.4g %s on %s", pick.v, canonUnit(p.Unit), pick.f.Effective.Format("2006-01-02"))
	return n
}

// filter applies code, status and recency filters.
func (e Evaluator) filter(p *Predicate, in evidence.Set) evidence.Set {
	codes := p.codes()
	var out evidence.Set
	for _, f := range in {
		if len(codes) > 0 && !matchAny(codes, f.Code) {
			continue
		}
		if len(p.Status) > 0 && !containsFold(p.Status, f.Status) {
			continue
		}
		if p.WithinDays > 0 {
			if f.Effective.IsZero() {
				continue
			}
			cutoff := e.asOf().AddDate(0, 0, -p.WithinDays)
			if f.Effective.Before(cutoff) || f.Effective.After(e.asOf()) {
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

func (e Evaluator) asOf() time.Time {
	if e.AsOf.IsZero() {
		return time.Now().UTC()
	}
	return e.AsOf
}

func compare(v float64, op string, want float64) bool {
	const eps = 1e-9
	switch op {
	case ">=":
		return v >= want-eps
	case ">":
		return v > want+eps
	case "<=":
		return v <= want+eps
	case "<":
		return v < want-eps
	case "==":
		return math.Abs(v-want) < eps
	case "!=":
		return math.Abs(v-want) >= eps
	}
	return false
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func ids(s evidence.Set) []string {
	out := make([]string, 0, len(s))
	for _, f := range s {
		out = append(out, f.ID)
	}
	sort.Strings(out)
	return out
}

func descOr(d, def string) string {
	if d != "" {
		return d
	}
	return def
}

func windowDesc(p *Predicate, minCount int) string {
	var parts []string
	if minCount > 1 {
		parts = append(parts, fmt.Sprintf("x%d", minCount))
	}
	if p.WithinDays > 0 {
		parts = append(parts, fmt.Sprintf("within %dd", p.WithinDays))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func durationDesc(p *Predicate) string {
	if p.MinDurationDays > 0 {
		return fmt.Sprintf(" for >= %d days", p.MinDurationDays)
	}
	return ""
}
