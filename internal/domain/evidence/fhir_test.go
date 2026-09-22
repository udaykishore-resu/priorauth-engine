package evidence_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
)

var asOf = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func loadBundle(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/bundle_lbp.json")
	require.NoError(t, err)
	return b
}

func TestFHIRExtractor_MapsAllResourceTypes(t *testing.T) {
	facts, err := evidence.FHIRExtractor{}.Extract(context.Background(), evidence.Input{Bundle: loadBundle(t), AsOf: asOf})
	require.NoError(t, err)

	byKind := map[evidence.Kind]int{}
	for _, f := range facts {
		byKind[f.Kind]++
		require.Equal(t, evidence.OriginStructured, f.Prov.Origin)
		require.Equal(t, evidence.FHIRExtractorName, f.Prov.Extractor)
		require.True(t, f.Usable(), "structured facts are always usable")
	}
	require.Equal(t, 2, byKind[evidence.KindDemographic], "age + sex")
	require.Equal(t, 2, byKind[evidence.KindDiagnosis])
	require.Equal(t, 2, byKind[evidence.KindObservation])
	require.Equal(t, 1, byKind[evidence.KindMedication])
	require.Equal(t, 4, byKind[evidence.KindProcedure])

	var age *evidence.Fact
	for i := range facts {
		if facts[i].Code == evidence.CodeAge {
			age = &facts[i]
		}
	}
	require.NotNil(t, age)
	require.Equal(t, 55.0, age.Value.Value, "born 1971-05-14, as of 2026-09-01")

	// Deterministic ordering: two runs produce identical output.
	again, err := evidence.FHIRExtractor{}.Extract(context.Background(), evidence.Input{Bundle: loadBundle(t), AsOf: asOf})
	require.NoError(t, err)
	require.Equal(t, facts, again)
}

func TestFHIRExtractor_DefaultsAndEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		in      evidence.Input
		wantErr bool
		want    int
	}{
		{name: "empty input yields nothing", in: evidence.Input{AsOf: asOf}, want: 0},
		{name: "birth date only yields age", in: evidence.Input{BirthDate: time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC), AsOf: asOf}, want: 1},
		{name: "invalid json", in: evidence.Input{Bundle: []byte("{nope")}, wantErr: true},
		{name: "not a bundle", in: evidence.Input{Bundle: []byte(`{"resourceType":"Patient"}`)}, wantErr: true},
		{name: "bad entry", in: evidence.Input{Bundle: []byte(`{"resourceType":"Bundle","entry":[{"resource":"x"}]}`)}, wantErr: true},
		{name: "unknown resources ignored", in: evidence.Input{Bundle: []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Organization","id":"o"}}]}`)}, want: 0},
		{name: "condition without codes", in: evidence.Input{Bundle: []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Condition","id":"c","code":{"text":"back pain"}}}]}`)}, want: 0},
		{name: "observation with codeable value", in: evidence.Input{Bundle: []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Observation","id":"o","status":"final","code":{"coding":[{"code":"X"}]},"valueCodeableConcept":{"coding":[{"code":"POS"}]}}}]}`)}, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts, err := evidence.FHIRExtractor{}.Extract(context.Background(), tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, facts, tc.want)
		})
	}
}

func TestAgeBoundaries(t *testing.T) {
	tests := []struct {
		birth, asOf string
		want        float64
	}{
		{"2008-09-01", "2026-09-01", 18}, // birthday today
		{"2008-09-02", "2026-09-01", 17}, // birthday tomorrow
		{"2008-02-29", "2026-02-28", 17}, // leap birthday not yet reached
		{"2008-02-29", "2026-03-01", 18},
		{"2030-01-01", "2026-09-01", 0}, // future birth date clamps to 0
	}
	for _, tc := range tests {
		birth, _ := time.Parse("2006-01-02", tc.birth)
		as, _ := time.Parse("2006-01-02", tc.asOf)
		facts, err := evidence.FHIRExtractor{}.Extract(context.Background(), evidence.Input{BirthDate: birth, AsOf: as})
		require.NoError(t, err)
		require.Len(t, facts, 1)
		require.Equal(t, tc.want, facts[0].Value.Value, "%s as of %s", tc.birth, tc.asOf)
	}
}

func TestUsableAndCorroborate(t *testing.T) {
	structured := evidence.Fact{ID: "s", Kind: evidence.KindDiagnosis, Code: "M54.5", Prov: evidence.Provenance{Origin: evidence.OriginStructured}}
	proposalMatch := evidence.Fact{ID: "p1", Kind: evidence.KindDiagnosis, Code: "m54.5", Prov: evidence.Provenance{Origin: evidence.OriginLLM}}
	proposalNew := evidence.Fact{ID: "p2", Kind: evidence.KindProcedure, Code: "97110", Prov: evidence.Provenance{Origin: evidence.OriginLLM}}
	human := evidence.Fact{ID: "h", Kind: evidence.KindProcedure, Code: "97140", Prov: evidence.Provenance{Origin: evidence.OriginHuman}}
	bogus := evidence.Fact{ID: "b", Prov: evidence.Provenance{Origin: "alien"}}

	require.False(t, proposalMatch.Usable())
	require.False(t, proposalNew.Usable())
	require.True(t, human.Usable())
	require.False(t, bogus.Usable())

	set, n := evidence.Corroborate(evidence.Set{structured, proposalMatch, proposalNew, human})
	require.Equal(t, 1, n)
	require.True(t, set[1].Prov.Corroborated)
	require.True(t, set[1].Usable(), "corroborated proposal becomes usable")
	require.False(t, set[2].Usable())
	require.Len(t, set.Proposals(), 1)
	require.Equal(t, "p2", set.Proposals()[0].ID)
	require.Len(t, set.Usable(), 3)

	// Confirmation makes the remaining proposal usable.
	set[2].Prov.ConfirmedBy = "dr.who"
	require.Empty(t, set.Proposals())
	require.Contains(t, set[2].String(), "procedure 97110")
}

func TestValidKind(t *testing.T) {
	require.True(t, evidence.ValidKind(evidence.KindObservation))
	require.False(t, evidence.ValidKind("vibes"))
}
