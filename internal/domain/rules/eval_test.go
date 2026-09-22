package rules

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
)

var asOf = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func f64(v float64) *float64 { return &v }

func structured(id string, kind evidence.Kind, code string, opts ...func(*evidence.Fact)) evidence.Fact {
	f := evidence.Fact{ID: id, Kind: kind, Code: code, Status: "active", Prov: evidence.Provenance{Origin: evidence.OriginStructured, Extractor: "test"}}
	for _, o := range opts {
		o(&f)
	}
	return f
}

func at(d string) func(*evidence.Fact) {
	return func(f *evidence.Fact) { f.Effective, _ = time.Parse("2006-01-02", d) }
}
func qty(v float64, u string) func(*evidence.Fact) {
	return func(f *evidence.Fact) { f.Value = &evidence.Quantity{Value: v, Unit: u} }
}
func text(s string) func(*evidence.Fact)   { return func(f *evidence.Fact) { f.Text = s } }
func status(s string) func(*evidence.Fact) { return func(f *evidence.Fact) { f.Status = s } }
func llm(f evidence.Fact) evidence.Fact {
	f.Prov.Origin = evidence.OriginLLM
	return f
}

func TestEvaluator_Leaves(t *testing.T) {
	age := structured("age", evidence.KindDemographic, evidence.CodeAge, qty(45, "a"))
	sex := structured("sex", evidence.KindDemographic, evidence.CodeSex, text("female"))
	dx := structured("dx1", evidence.KindDiagnosis, "M54.16", at("2026-03-01"))
	oldDx := structured("dx2", evidence.KindDiagnosis, "M51.16", at("2020-01-01"))
	pt := func(id, d string) evidence.Fact {
		return structured(id, evidence.KindProcedure, "97110", at(d), status("completed"))
	}
	met := structured("met", evidence.KindMedication, "6809", at("2026-04-01"))
	metUndated := structured("met2", evidence.KindMedication, "6809")
	a1c := structured("a1c", evidence.KindObservation, "4548-4", at("2026-07-01"), qty(58, "mmol/mol"))
	a1cOld := structured("a1c-old", evidence.KindObservation, "4548-4", at("2025-01-01"), qty(9.5, "%"))
	bmiNoUnit := structured("bmi", evidence.KindObservation, "39156-5", at("2026-08-01"), qty(31, ""))
	weightLb := structured("wt", evidence.KindObservation, "29463-7", at("2026-08-01"), qty(220, "lb"))
	glucoseStr := structured("glu", evidence.KindObservation, "2345-7", at("2026-08-01"), text("high"))

	tests := []struct {
		name  string
		p     Predicate
		facts evidence.Set
		want  Outcome
		ev    []string
	}{
		{"age met", Predicate{Fact: FactAge, Op: ">=", Value: f64(18)}, evidence.Set{age}, Met, []string{"age"}},
		{"age not met", Predicate{Fact: FactAge, Op: "<", Value: f64(18)}, evidence.Set{age}, NotMet, []string{"age"}},
		{"age unknown", Predicate{Fact: FactAge, Op: ">=", Value: f64(18)}, evidence.Set{sex}, Unknown, nil},
		{"age equality with epsilon", Predicate{Fact: FactAge, Op: "==", Value: f64(45)}, evidence.Set{age}, Met, []string{"age"}},
		{"sex met case-insensitive", Predicate{Fact: FactSex, Equals: "FEMALE"}, evidence.Set{sex}, Met, []string{"sex"}},
		{"sex not met", Predicate{Fact: FactSex, Equals: "male"}, evidence.Set{sex}, NotMet, []string{"sex"}},
		{"sex unknown", Predicate{Fact: FactSex, Equals: "male"}, evidence.Set{age}, Unknown, nil},

		{"dx exact", Predicate{Fact: FactDiagnosis, Codes: []string{"M54.16"}}, evidence.Set{dx}, Met, []string{"dx1"}},
		{"dx prefix pattern", Predicate{Fact: FactDiagnosis, Codes: []string{"M54.*"}}, evidence.Set{dx}, Met, []string{"dx1"}},
		{"dx absent but history present", Predicate{Fact: FactDiagnosis, Codes: []string{"E11.*"}}, evidence.Set{dx}, NotMet, nil},
		{"dx no history", Predicate{Fact: FactDiagnosis, Codes: []string{"E11.*"}}, evidence.Set{age}, Unknown, nil},
		{"dx recency window excludes old", Predicate{Fact: FactDiagnosis, Codes: []string{"M51.*"}, WithinDays: 365}, evidence.Set{oldDx}, NotMet, nil},
		{"dx status filter", Predicate{Fact: FactDiagnosis, Codes: []string{"M54.*"}, Status: []string{"resolved"}}, evidence.Set{dx}, NotMet, nil},
		{"dx min_count", Predicate{Fact: FactDiagnosis, Codes: []string{"M*"}, MinCount: 2}, evidence.Set{dx, oldDx}, Met, []string{"dx1", "dx2"}},

		{"procedure count met", Predicate{Fact: FactProcedure, Codes: []string{"97110"}, MinCount: 3, WithinDays: 180},
			evidence.Set{pt("a", "2026-06-01"), pt("b", "2026-07-01"), pt("c", "2026-08-01"), pt("old", "2025-01-01")}, Met, []string{"a", "b", "c"}},
		{"procedure count short", Predicate{Fact: FactProcedure, Codes: []string{"97110"}, MinCount: 3, WithinDays: 180},
			evidence.Set{pt("a", "2026-06-01"), pt("old", "2025-01-01")}, NotMet, []string{"a"}},
		{"procedure future-dated excluded", Predicate{Fact: FactProcedure, Codes: []string{"97110"}, WithinDays: 30},
			evidence.Set{pt("fut", "2027-01-01")}, NotMet, nil},

		{"medication present", Predicate{Fact: FactMedication, Codes: []string{"6809"}}, evidence.Set{met}, Met, []string{"met"}},
		{"medication duration met", Predicate{Fact: FactMedication, Codes: []string{"6809"}, MinDurationDays: 90}, evidence.Set{met}, Met, []string{"met"}},
		{"medication duration short", Predicate{Fact: FactMedication, Codes: []string{"6809"}, MinDurationDays: 200}, evidence.Set{met}, NotMet, []string{"met"}},
		{"medication duration undated", Predicate{Fact: FactMedication, Codes: []string{"6809"}, MinDurationDays: 90}, evidence.Set{metUndated}, Unknown, []string{"met2"}},
		{"medication wrong drug", Predicate{Fact: FactMedication, Codes: []string{"7258"}}, evidence.Set{met}, NotMet, nil},
		{"medication no history", Predicate{Fact: FactMedication, Codes: []string{"7258"}}, evidence.Set{age}, Unknown, nil},

		{"obs unit conversion mmol/mol -> % (58 -> 7.46)", Predicate{Fact: FactObservation, Code: "4548-4", Op: ">=", Value: f64(7), Unit: "%"}, evidence.Set{a1c}, Met, []string{"a1c"}},
		{"obs latest wins over older higher", Predicate{Fact: FactObservation, Code: "4548-4", Op: ">=", Value: f64(9), Unit: "%"}, evidence.Set{a1cOld, a1c}, NotMet, []string{"a1c"}},
		{"obs max aggregate", Predicate{Fact: FactObservation, Code: "4548-4", Op: ">=", Value: f64(9), Unit: "%", Aggregate: "max"}, evidence.Set{a1cOld, a1c}, Met, []string{"a1c-old"}},
		{"obs min aggregate", Predicate{Fact: FactObservation, Code: "4548-4", Op: "<", Value: f64(8), Unit: "%", Aggregate: "min"}, evidence.Set{a1cOld, a1c}, Met, []string{"a1c"}},
		{"obs window excludes old", Predicate{Fact: FactObservation, Code: "4548-4", Op: ">=", Value: f64(9), Unit: "%", WithinDays: 180}, evidence.Set{a1cOld}, Unknown, nil},
		{"obs presence only", Predicate{Fact: FactObservation, Code: "4548-4"}, evidence.Set{a1c}, Met, []string{"a1c"}},
		{"obs missing", Predicate{Fact: FactObservation, Code: "39156-5", Op: ">=", Value: f64(30), Unit: "kg/m2"}, evidence.Set{a1c}, Unknown, nil},
		{"obs no unit on fact", Predicate{Fact: FactObservation, Code: "39156-5", Op: ">=", Value: f64(30), Unit: "kg/m2"}, evidence.Set{bmiNoUnit}, Unknown, nil},
		{"obs generic mass conversion lb -> kg", Predicate{Fact: FactObservation, Code: "29463-7", Op: ">=", Value: f64(99), Unit: "kg"}, evidence.Set{weightLb}, Met, []string{"wt"}},
		{"obs unconvertible", Predicate{Fact: FactObservation, Code: "29463-7", Op: ">=", Value: f64(1), Unit: "furlong"}, evidence.Set{weightLb}, Unknown, nil},
		{"obs non-numeric", Predicate{Fact: FactObservation, Code: "2345-7", Op: ">=", Value: f64(1), Unit: "mg/dL"}, evidence.Set{glucoseStr}, Unknown, nil},

		{"llm proposal ignored", Predicate{Fact: FactDiagnosis, Codes: []string{"M54.*"}}, evidence.Set{llm(dx), oldDx}, NotMet, nil},
		{"unsupported leaf", Predicate{Fact: "vibes"}, evidence.Set{dx}, Unknown, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := Evaluator{AsOf: asOf}.Evaluate(&tc.p, tc.facts)
			require.Equal(t, tc.want, n.Outcome, "reason: %s", n.Reason)
			if tc.ev == nil {
				require.Empty(t, n.Evidence)
			} else {
				require.Equal(t, tc.ev, n.Evidence)
			}
			require.NotEmpty(t, n.Description)
		})
	}
}

func TestEvaluator_Combinators(t *testing.T) {
	yes := Predicate{Fact: FactAge, Op: ">=", Value: f64(1)}
	no := Predicate{Fact: FactAge, Op: "<", Value: f64(1)}
	unk := Predicate{Fact: FactSex, Equals: "female"} // no sex fact → unknown
	facts := evidence.Set{structured("age", evidence.KindDemographic, evidence.CodeAge, qty(30, "a"))}

	tests := []struct {
		name string
		p    Predicate
		want Outcome
	}{
		{"all met", Predicate{All: []Predicate{yes, yes}}, Met},
		{"all with not-met", Predicate{All: []Predicate{yes, no, unk}}, NotMet},
		{"all with unknown", Predicate{All: []Predicate{yes, unk}}, Unknown},
		{"any met beats unknown", Predicate{Any: []Predicate{unk, yes}}, Met},
		{"any unknown", Predicate{Any: []Predicate{no, unk}}, Unknown},
		{"any not met", Predicate{Any: []Predicate{no, no}}, NotMet},
		{"not met→not_met", Predicate{Not: &yes}, NotMet},
		{"not not_met→met", Predicate{Not: &no}, Met},
		{"not unknown→unknown", Predicate{Not: &unk}, Unknown},
		{"at_least met", Predicate{AtLeast: &AtLeast{N: 2, Of: []Predicate{yes, yes, no}}}, Met},
		{"at_least unknown could still reach", Predicate{AtLeast: &AtLeast{N: 2, Of: []Predicate{yes, unk, no}}}, Unknown},
		{"at_least not met", Predicate{AtLeast: &AtLeast{N: 2, Of: []Predicate{yes, no, no}}}, NotMet},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := Evaluator{AsOf: asOf}.Evaluate(&tc.p, facts)
			require.Equal(t, tc.want, n.Outcome)
		})
	}

	t.Run("nil criteria is met", func(t *testing.T) {
		require.Equal(t, Met, Evaluator{}.Evaluate(nil, nil).Outcome)
	})
	t.Run("missing data lists unknown leaves only", func(t *testing.T) {
		n := Evaluator{AsOf: asOf}.Evaluate(&Predicate{All: []Predicate{yes, unk, {Not: &unk}}}, facts)
		require.Equal(t, []string{"sex is female", "sex is female"}, n.MissingData())
	})
}

func TestConvert(t *testing.T) {
	tests := []struct {
		code     string
		v        float64
		from, to string
		want     float64
		err      bool
	}{
		{"", 1, "kg", "lb", 2.20462262, false},
		{"", 100, "lb", "kg", 45.359237, false},
		{"", 72, "in", "cm", 182.88, false},
		{"", 30, "kg/m^2", "kg/m2", 30, false},
		{"", 30, "kg/m²", "kg/m2", 30, false},
		{"2345-7", 180, "mg/dL", "mmol/L", 9.9899, false},
		{"2345-7", 10, "mmol/L", "mg/dl", 180.182, false},
		{"4548-4", 7.0, "%", "mmol/mol", 53.0, false},
		{"4548-4", 53.0, "mmol/mol", "%", 7.0, false},
		{"", 5, "", "kg", 0, true},
		{"", 5, "kg", "", 5, false},
		{"", 5, "kg", "L", 0, true},
		{"29463-7", 5, "mg/dL", "mmol/L", 0, true}, // analyte-specific factor unknown for this code
	}
	for _, tc := range tests {
		got, err := convert(tc.code, tc.v, tc.from, tc.to)
		if tc.err {
			require.Error(t, err, "%v", tc)
			continue
		}
		require.NoError(t, err, "%v", tc)
		require.InDelta(t, tc.want, got, 0.01, "%v", tc)
	}
}
