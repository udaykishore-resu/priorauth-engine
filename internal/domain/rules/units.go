package rules

import (
	"fmt"
	"strings"
)

// conversion is value_to = value_from*Factor + Offset.
type conversion struct {
	Factor, Offset float64
}

// unitKey is (observation code, from unit, to unit). Code "" is the generic
// fallback used when the pair is not analyte-specific.
type unitKey struct{ code, from, to string }

// conversions is a deliberately small, explicit table. Analyte-specific
// pairs (glucose mg/dL <-> mmol/L, HbA1c % <-> mmol/mol) are keyed by LOINC
// code because the factor depends on molar mass; generic pairs are physical
// units that convert independent of analyte.
var conversions = map[unitKey]conversion{
	// mass
	{"", "kg", "lb"}: {Factor: 2.20462262},
	{"", "lb", "kg"}: {Factor: 0.45359237},
	{"", "g", "kg"}:  {Factor: 0.001},
	{"", "kg", "g"}:  {Factor: 1000},
	// length
	{"", "cm", "in"}: {Factor: 0.393700787},
	{"", "in", "cm"}: {Factor: 2.54},
	{"", "m", "cm"}:  {Factor: 100},
	{"", "cm", "m"}:  {Factor: 0.01},
	// time
	{"", "a", "mo"}: {Factor: 12},
	{"", "mo", "a"}: {Factor: 1.0 / 12},
	// glucose (LOINC 2345-7 serum/plasma, 2339-0 blood, 1558-6 fasting)
	{"2345-7", "mg/dL", "mmol/L"}: {Factor: 1.0 / 18.0182},
	{"2345-7", "mmol/L", "mg/dL"}: {Factor: 18.0182},
	{"2339-0", "mg/dL", "mmol/L"}: {Factor: 1.0 / 18.0182},
	{"2339-0", "mmol/L", "mg/dL"}: {Factor: 18.0182},
	{"1558-6", "mg/dL", "mmol/L"}: {Factor: 1.0 / 18.0182},
	{"1558-6", "mmol/L", "mg/dL"}: {Factor: 18.0182},
	// HbA1c NGSP % <-> IFCC mmol/mol: IFCC = (NGSP - 2.15) * 10.929
	{"4548-4", "%", "mmol/mol"}: {Factor: 10.929, Offset: -2.15 * 10.929},
	{"4548-4", "mmol/mol", "%"}: {Factor: 1 / 10.929, Offset: 2.15},
	// LDL cholesterol (LOINC 13457-7, 2089-1) mg/dL <-> mmol/L
	{"2089-1", "mg/dL", "mmol/L"}:  {Factor: 1.0 / 38.67},
	{"2089-1", "mmol/L", "mg/dL"}:  {Factor: 38.67},
	{"13457-7", "mg/dL", "mmol/L"}: {Factor: 1.0 / 38.67},
	{"13457-7", "mmol/L", "mg/dL"}: {Factor: 38.67},
}

// unitAliases folds common spellings onto the canonical UCUM-ish token.
var unitAliases = map[string]string{
	"kg/m2": "kg/m2", "kg/m^2": "kg/m2", "kg/m²": "kg/m2",
	"mg/dl": "mg/dL", "mg/dL": "mg/dL",
	"mmol/l": "mmol/L", "mmol/L": "mmol/L",
	"years": "a", "year": "a", "yr": "a", "a": "a",
	"months": "mo", "month": "mo", "mo": "mo",
	"lbs": "lb", "lb": "lb", "[lb_av]": "lb",
	"inches": "in", "inch": "in", "in": "in", "[in_i]": "in",
	"percent": "%", "%": "%",
	"mmhg": "mm[Hg]", "mm[hg]": "mm[Hg]", "mm[Hg]": "mm[Hg]",
	"kg": "kg", "g": "g", "cm": "cm", "m": "m", "mmol/mol": "mmol/mol",
}

func canonUnit(u string) string {
	u = strings.TrimSpace(u)
	if c, ok := unitAliases[u]; ok {
		return c
	}
	if c, ok := unitAliases[strings.ToLower(u)]; ok {
		return c
	}
	return u
}

// convert expresses value (in from) in unit to for the given observation
// code. Same-unit (after aliasing) is identity. An unknown pair is an error,
// which the evaluator surfaces as an Unknown outcome rather than guessing.
func convert(code string, value float64, from, to string) (float64, error) {
	f, t := canonUnit(from), canonUnit(to)
	if f == t || t == "" {
		return value, nil
	}
	if f == "" {
		return 0, fmt.Errorf("fact has no unit; rule expects %q", to)
	}
	if c, ok := conversions[unitKey{code, f, t}]; ok {
		return value*c.Factor + c.Offset, nil
	}
	if c, ok := conversions[unitKey{"", f, t}]; ok {
		return value*c.Factor + c.Offset, nil
	}
	return 0, fmt.Errorf("no conversion from %q to %q for code %q", from, to, code)
}
