package workflow

import (
	"strings"
	"time"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/rules"
)

// RuleRequest maps the intake onto the rule selector input.
func (r *Request) RuleRequest() rules.Request {
	return rules.Request{
		Payer:          r.Intake.Payer.ID,
		Plan:           r.Intake.Payer.Plan,
		ServiceCode:    r.Intake.Service.Code,
		DiagnosisCodes: r.Intake.Service.Diagnoses,
		ServiceDate:    r.Intake.Service.ServiceDate,
	}
}

// ApplyReviews stamps human confirmations/rejections onto extracted facts:
// confirmed proposals become usable, rejected ones are dropped.
func (r *Request) ApplyReviews(facts evidence.Set) evidence.Set {
	out := make(evidence.Set, 0, len(facts))
	for _, f := range facts {
		if f.Prov.Origin == evidence.OriginLLM {
			if _, rejected := r.RejectedFacts[f.ID]; rejected {
				continue
			}
			if actor, ok := r.ConfirmedFacts[f.ID]; ok {
				f.Prov.ConfirmedBy = actor
			}
		}
		out = append(out, f)
	}
	return out
}

// BuildPackage assembles the evidence package: it corroborates LLM
// proposals against structured facts, applies human reviews, re-evaluates
// the governing rule's criteria over the usable facts and fills the
// documentation checklist from attachments and notes.
func (r *Request) BuildPackage(set *rules.Set, facts evidence.Set, extractors []string, at time.Time) Package {
	facts, _ = evidence.Corroborate(r.ApplyReviews(facts))
	facts = facts.Sorted()

	p := Package{Facts: facts, Extractors: extractors, AssembledAt: at, Proposals: len(facts.Proposals())}

	rule, ok := set.Select(r.RuleRequest())
	if !ok {
		p.Criteria = rules.Unknown
		p.MissingData = []string{"no coverage rule matched"}
		p.Explain = &rules.Node{Description: "no coverage rule matched", Outcome: rules.Unknown}
	} else {
		tree := rules.Evaluator{AsOf: at}.Evaluate(rule.Criteria, facts)
		p.Criteria = tree.Outcome
		p.Explain = &tree
		p.MissingData = tree.MissingData()
		for _, d := range rule.Documentation {
			st := DocStatus{Code: d.Code, Display: d.Display, Optional: d.Optional}
			st.Satisfied, st.Source = r.documentationSource(d.Code)
			p.Documentation = append(p.Documentation, st)
		}
	}

	docsOK := true
	for _, d := range p.Documentation {
		if !d.Optional && !d.Satisfied {
			docsOK = false
		}
	}
	p.Complete = docsOK && (p.Criteria == rules.Met || r.Override != nil)
	return p
}

// documentationSource finds an attachment or typed note that satisfies a
// documentation code.
func (r *Request) documentationSource(code string) (bool, string) {
	for _, a := range r.Intake.Attachments {
		if strings.EqualFold(a.Type, code) {
			return true, a.Ref
		}
	}
	for _, n := range r.Intake.Notes {
		if strings.EqualFold(n.Type, code) {
			return true, "note:" + n.ID
		}
	}
	return false, ""
}
