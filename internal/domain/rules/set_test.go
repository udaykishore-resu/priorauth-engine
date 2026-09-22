package rules

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
)

var update = flag.Bool("update", false, "rewrite golden files")

// shippedRulesDir points at the rule files the binary ships with, so the
// tests guard the real policy content, not a copy.
const shippedRulesDir = "../../../rules"

func loadShipped(t *testing.T) *Set {
	t.Helper()
	set, err := DirLoader{Dir: shippedRulesDir, Now: func() time.Time { return asOf }}.Load()
	require.NoError(t, err)
	return set
}

func TestLoadShippedRules(t *testing.T) {
	set := loadShipped(t)
	require.GreaterOrEqual(t, len(set.Rules), 5)
	require.Len(t, set.Hash, 16)
	// Hash is content-derived and stable across loads.
	again := loadShipped(t)
	require.Equal(t, set.Hash, again.Hash)
}

func TestSelect_Specificity(t *testing.T) {
	set := loadShipped(t)
	svc := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		req  Request
		want string
		ok   bool
	}{
		{"MRI lumbar", Request{Payer: "ACME_HEALTH", Plan: "HMO-SILVER", ServiceCode: "72148", ServiceDate: svc}, "acme.imaging.mri-lumbar-spine", true},
		{"payer case-insensitive", Request{Payer: "acme_health", Plan: "X", ServiceCode: "72158", ServiceDate: svc}, "acme.imaging.mri-lumbar-spine", true},
		{"PT generic plan", Request{Payer: "ACME_HEALTH", Plan: "HMO-SILVER", ServiceCode: "97110", ServiceDate: svc}, "acme.rehab.physical-therapy", true},
		{"PT plan-specific waiver wins", Request{Payer: "ACME_HEALTH", Plan: "PPO-PLATINUM", ServiceCode: "97110", ServiceDate: svc}, "acme.rehab.physical-therapy.ppo-platinum-waiver", true},
		{"E&M pattern", Request{Payer: "ACME_HEALTH", Plan: "X", ServiceCode: "99214", ServiceDate: svc}, "acme.professional.office-visits", true},
		{"before effective date", Request{Payer: "ACME_HEALTH", Plan: "X", ServiceCode: "72148", ServiceDate: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)}, "", false},
		{"unknown payer", Request{Payer: "OTHER", Plan: "X", ServiceCode: "72148", ServiceDate: svc}, "", false},
		{"unknown service", Request{Payer: "ACME_HEALTH", Plan: "X", ServiceCode: "00000", ServiceDate: svc}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := set.Select(tc.req)
			require.Equal(t, tc.ok, ok)
			if ok {
				require.Equal(t, tc.want, r.ID)
			}
		})
	}
}

func TestSelect_TieBreaks(t *testing.T) {
	base := Rule{Payer: "P", ServiceCodes: []string{"1"}, RequiresAuth: false}
	mk := func(id string, v int, mut func(*Rule)) Rule {
		r := base
		r.ID, r.Version = id, v
		if mut != nil {
			mut(&r)
		}
		return r
	}
	set, err := NewSet([]Rule{
		mk("b", 1, nil),
		mk("a", 1, nil),
		mk("a", 2, nil),
		mk("scoped", 1, func(r *Rule) { r.DiagnosisScope = []string{"M54.*"} }),
		mk("pattern", 1, func(r *Rule) { r.ServiceCodes = []string{"1*"} }),
		mk("wild", 9, func(r *Rule) { r.Payer = Wildcard }),
	}, asOf)
	require.NoError(t, err)

	r, ok := set.Select(Request{Payer: "P", ServiceCode: "1"})
	require.True(t, ok)
	require.Equal(t, "a@2", r.Ref(), "highest version, then lexical id")

	r, ok = set.Select(Request{Payer: "P", ServiceCode: "1", DiagnosisCodes: []string{"M54.5"}})
	require.True(t, ok)
	require.Equal(t, "scoped", r.ID, "diagnosis scope is more specific")

	r, ok = set.Select(Request{Payer: "P", ServiceCode: "12"})
	require.True(t, ok)
	require.Equal(t, "pattern", r.ID, "only the pattern matches 12")

	r, ok = set.Select(Request{Payer: "Q", ServiceCode: "1"})
	require.True(t, ok)
	require.Equal(t, "wild", r.ID, "wildcard payer is the fallback")
}

func TestNewSet_Rejects(t *testing.T) {
	good := Rule{ID: "x", Version: 1, Payer: "P", ServiceCodes: []string{"1"}}
	_, err := NewSet([]Rule{good, good}, asOf)
	require.ErrorContains(t, err, "duplicate rule x@1")

	bad := good
	bad.Version = 0
	_, err = NewSet([]Rule{bad}, asOf)
	require.ErrorContains(t, err, "version must be > 0")

	_, err = LoadFS(os.DirFS(t.TempDir()), ".", asOf)
	require.ErrorContains(t, err, "no rule files")
}

func TestParseRule_Validation(t *testing.T) {
	tests := []struct {
		name string
		json string
		err  string
	}{
		{"unknown field", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"bogus":true}`, "unknown field"},
		{"missing payer", `{"id":"a","version":1,"service_codes":["1"]}`, "payer is required"},
		{"empty service code", `{"id":"a","version":1,"payer":"P","service_codes":[""]}`, "empty code"},
		{"auth without criteria/docs", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"requires_auth":true}`, "must declare criteria or documentation"},
		{"two combinators", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"all":[{"fact":"age","op":">=","value":1}],"any":[{"fact":"age","op":">=","value":1}]}}`, "exactly one of"},
		{"age without op", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"fact":"age"}}`, "age requires op and value"},
		{"bad op", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"fact":"age","op":"~","value":1}}`, "age requires op and value"},
		{"sex without equals", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"fact":"sex"}}`, "sex requires equals"},
		{"dx without codes", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"fact":"diagnosis"}}`, "diagnosis requires codes"},
		{"duration on procedure", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"fact":"procedure","codes":["1"],"min_duration_days":3}}`, "only valid for medication"},
		{"obs bad aggregate", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"fact":"observation","code":"1","aggregate":"avg"}}`, "aggregate must be"},
		{"obs op without value", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"fact":"observation","code":"1","op":">="}}`, "requires a valid op and value"},
		{"unknown fact", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"fact":"mood"}}`, "unknown fact type"},
		{"at_least n out of range", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"at_least":{"n":3,"of":[{"fact":"age","op":">","value":1}]}}}`, "n must be within"},
		{"nested not invalid", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"not":{"fact":"age"}}}`, "criteria.not"},
		{"negative window", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"fact":"diagnosis","codes":["1"],"within_days":-1}}`, "negative window"},
		{"duplicate documentation", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"documentation":[{"code":"n"},{"code":"n"}]}`, "duplicated"},
		{"effective range inverted", `{"id":"a","version":1,"payer":"P","service_codes":["1"],"effective_from":"2026-02-01T00:00:00Z","effective_to":"2026-01-01T00:00:00Z"}`, "effective_to must be after"},
		{"not json", `{`, "decode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRule([]byte(tc.json))
			require.ErrorContains(t, err, tc.err)
		})
	}
	t.Run("plan defaults to wildcard", func(t *testing.T) {
		r, err := ParseRule([]byte(`{"id":"a","version":1,"payer":"P","service_codes":["1"]}`))
		require.NoError(t, err)
		require.Equal(t, Wildcard, r.Plan)
	})
}

// Golden test: the explain tree for a realistic MRI request must not drift
// without a deliberate update, since reviewers and payers read it.
func TestDetermine_GoldenExplainTree(t *testing.T) {
	set := loadShipped(t)
	bundle, err := os.ReadFile("../evidence/testdata/bundle_lbp.json")
	require.NoError(t, err)
	facts, err := evidence.FHIRExtractor{}.Extract(context.Background(), evidence.Input{Bundle: bundle, AsOf: asOf})
	require.NoError(t, err)

	d := set.Determine(Request{Payer: "ACME_HEALTH", Plan: "HMO-SILVER", ServiceCode: "72148", DiagnosisCodes: []string{"M54.16"}, ServiceDate: asOf}, facts, asOf)
	require.Equal(t, Required, d.Decision)
	require.Equal(t, "acme.imaging.mri-lumbar-spine@3", d.RuleRef)
	// 4 PT visits (<6) but naproxen since 2026-05-10 (>42 days) → conservative therapy met.
	require.Equal(t, Met, d.Criteria, "explain: %+v", d.Explain)
	require.Len(t, d.Documentation, 4)

	d.RuleSetHash = "<hash>" // content hash is asserted elsewhere; keep golden stable
	got, err := json.MarshalIndent(d, "", "  ")
	require.NoError(t, err)
	golden := filepath.Join("testdata", "mri_lbp_explain.golden.json")
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(golden, got, 0o644))
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err, "run with -update to create the golden file")
	require.JSONEq(t, string(want), string(got))
}

func TestDetermine_Outcomes(t *testing.T) {
	set := loadShipped(t)
	adult := evidence.Set{structured("age", evidence.KindDemographic, evidence.CodeAge, qty(40, "a"))}
	t.Run("not required", func(t *testing.T) {
		d := set.Determine(Request{Payer: "ACME_HEALTH", ServiceCode: "99213", ServiceDate: asOf}, adult, asOf)
		require.Equal(t, NotRequired, d.Decision)
		require.Equal(t, "acme.professional.office-visits@1", d.RuleRef)
		require.Nil(t, d.Explain)
	})
	t.Run("indeterminate when no rule", func(t *testing.T) {
		d := set.Determine(Request{Payer: "NOBODY", ServiceCode: "72148", ServiceDate: asOf}, adult, asOf)
		require.Equal(t, Indeterminate, d.Decision)
		require.Equal(t, "none", d.RuleRef)
		require.NotEmpty(t, d.MissingData)
	})
	t.Run("required with unknown criteria lists missing data", func(t *testing.T) {
		d := set.Determine(Request{Payer: "ACME_HEALTH", ServiceCode: "72148", ServiceDate: asOf}, adult, asOf)
		require.Equal(t, Required, d.Decision)
		require.Equal(t, Unknown, d.Criteria)
		require.Contains(t, d.MissingData, "qualifying lumbar diagnosis")
	})
	t.Run("GLP-1 T2DM pathway", func(t *testing.T) {
		facts := append(evidence.Set{}, adult...)
		facts = append(facts,
			structured("dx", evidence.KindDiagnosis, "E11.9", at("2024-01-01")),
			structured("a1c", evidence.KindObservation, "4548-4", at("2026-07-15"), qty(8.1, "%")),
			structured("met", evidence.KindMedication, "6809", at("2026-01-01")),
		)
		d := set.Determine(Request{Payer: "ACME_HEALTH", ServiceCode: "1991302", ServiceDate: asOf}, facts, asOf)
		require.Equal(t, Required, d.Decision)
		require.Equal(t, Met, d.Criteria, "%v", d.MissingData)
	})
	t.Run("GLP-1 contraindication blocks", func(t *testing.T) {
		facts := append(evidence.Set{}, adult...)
		facts = append(facts,
			structured("dx", evidence.KindDiagnosis, "E11.9", at("2024-01-01")),
			structured("mtc", evidence.KindDiagnosis, "C73", at("2020-01-01")),
			structured("a1c", evidence.KindObservation, "4548-4", at("2026-07-15"), qty(8.1, "%")),
			structured("met", evidence.KindMedication, "6809", at("2026-01-01")),
		)
		d := set.Determine(Request{Payer: "ACME_HEALTH", ServiceCode: "1991302", ServiceDate: asOf}, facts, asOf)
		require.Equal(t, NotMet, d.Criteria)
	})
	t.Run("PT visit cap", func(t *testing.T) {
		facts := append(evidence.Set{}, adult...)
		facts = append(facts,
			structured("dx", evidence.KindDiagnosis, "M25.561", at("2026-06-01")),
			structured("eval", evidence.KindProcedure, "97162", at("2026-08-01")),
		)
		for i := range 13 {
			facts = append(facts, structured("v"+string(rune('a'+i)), evidence.KindProcedure, "97110", at("2026-08-15")))
		}
		d := set.Determine(Request{Payer: "ACME_HEALTH", Plan: "HMO", ServiceCode: "97110", ServiceDate: asOf}, facts, asOf)
		require.Equal(t, NotMet, d.Criteria)
		d = set.Determine(Request{Payer: "ACME_HEALTH", Plan: "HMO", ServiceCode: "97110", ServiceDate: asOf}, facts[:6], asOf)
		require.Equal(t, Met, d.Criteria)
	})
}

func TestStore_ReplaceAndWatch(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}
	write("a.json", `{"id":"a","version":1,"payer":"P","service_codes":["1"]}`)
	loader := DirLoader{Dir: dir}
	initial, err := loader.Load()
	require.NoError(t, err)

	store := NewStore(initial)
	swaps := make(chan string, 4)
	store.OnSwap(func(_, next *Set) { swaps <- next.Hash })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.Watch(ctx, loader, 20*time.Millisecond, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	}()

	// A broken file must not replace the good set.
	time.Sleep(30 * time.Millisecond)
	write("a.json", `{"id":"a","version":0,"payer":"P","service_codes":["1"]}`)
	select {
	case h := <-swaps:
		t.Fatalf("unexpected swap to %s on invalid rules", h)
	case <-time.After(150 * time.Millisecond):
	}
	require.Equal(t, initial.Hash, store.Current().Hash)

	// A valid change hot-reloads.
	write("a.json", `{"id":"a","version":2,"payer":"P","service_codes":["1"]}`)
	select {
	case h := <-swaps:
		require.NotEqual(t, initial.Hash, h)
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not reload")
	}
	require.Equal(t, 2, store.Current().Rules[0].Version)
	require.Equal(t, int64(1), store.Reloads())

	store.Replace(nil) // no-op
	require.Equal(t, int64(1), store.Reloads())

	cancel()
	<-done
}

func FuzzParseRule(f *testing.F) {
	seeds, _ := filepath.Glob(filepath.Join(shippedRulesDir, "*.json"))
	for _, s := range seeds {
		b, err := os.ReadFile(s)
		if err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte(`{"id":"a","version":1,"payer":"P","service_codes":["1"],"criteria":{"not":{"fact":"age","op":">","value":1}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := ParseRule(data)
		if err != nil {
			return
		}
		// Whatever parsed must validate again, evaluate without panicking and
		// round-trip through NewSet.
		require.NoError(t, r.Validate())
		facts := evidence.Set{
			structured("age", evidence.KindDemographic, evidence.CodeAge, qty(40, "a")),
			structured("dx", evidence.KindDiagnosis, "M54.5", at("2026-01-01")),
			structured("obs", evidence.KindObservation, "4548-4", at("2026-01-01"), qty(7, "%")),
		}
		_ = Evaluator{AsOf: asOf}.Evaluate(r.Criteria, facts)
		_, err = NewSet([]Rule{r}, asOf)
		require.NoError(t, err)
	})
}
