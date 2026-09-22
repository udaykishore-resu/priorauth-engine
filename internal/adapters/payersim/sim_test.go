package payersim_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/payerhttp"
	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/payersim"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/pas"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
)

var now = time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)

func bundle(evidence int, diagnoses int) []byte {
	var info, dx []map[string]any
	for i := 0; i < evidence; i++ {
		info = append(info, map[string]any{"sequence": i + 1})
	}
	for i := 0; i < diagnoses; i++ {
		dx = append(dx, map[string]any{"sequence": i + 1})
	}
	claim := map[string]any{"resourceType": "Claim", "id": "claim-1", "patient": map[string]string{"reference": "Patient/p"}, "supportingInfo": info, "diagnosis": dx}
	b, _ := json.Marshal(map[string]any{"resourceType": "Bundle", "entry": []map[string]any{{"resource": claim}}})
	return b
}

func TestEngineModes(t *testing.T) {
	tests := []struct {
		name   string
		mode   payersim.Mode
		bundle []byte
		want   workflow.PayerOutcome
	}{
		{"approve", payersim.ModeApprove, bundle(0, 1), workflow.OutcomeApproved},
		{"deny", payersim.ModeDeny, bundle(9, 1), workflow.OutcomeDenied},
		{"pend", payersim.ModePend, bundle(9, 1), workflow.OutcomePended},
		{"smart rich", payersim.ModeSmart, bundle(8, 1), workflow.OutcomeApproved},
		{"smart thin", payersim.ModeSmart, bundle(3, 1), workflow.OutcomePended},
		{"smart no dx", payersim.ModeSmart, bundle(9, 0), workflow.OutcomeDenied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := payersim.DefaultConfig()
			cfg.Mode = tc.mode
			e, err := payersim.New(cfg, func() time.Time { return now })
			require.NoError(t, err)
			raw, err := e.Submit(tc.bundle, "")
			require.NoError(t, err)
			d, err := pas.Interpret(raw, now)
			require.NoError(t, err)
			require.Equal(t, tc.want, d.Outcome)
			require.True(t, strings.HasPrefix(d.PayerRef, "SIM-"))
		})
	}
}

func TestEngineForcedInquireAndRandom(t *testing.T) {
	cfg := payersim.DefaultConfig()
	cfg.PendFlips = 2
	e, err := payersim.New(cfg, func() time.Time { return now })
	require.NoError(t, err)

	raw, err := e.Submit(bundle(9, 1), "deny")
	require.NoError(t, err)
	d, _ := pas.Interpret(raw, now)
	require.Equal(t, workflow.OutcomeDenied, d.Outcome)
	require.NotEmpty(t, d.Reasons)

	raw, err = e.Submit(bundle(9, 1), "pend")
	require.NoError(t, err)
	d, _ = pas.Interpret(raw, now)
	require.Equal(t, workflow.OutcomePended, d.Outcome)
	ref := d.PayerRef

	raw, err = e.Inquire(ref)
	require.NoError(t, err)
	d, _ = pas.Interpret(raw, now)
	require.Equal(t, workflow.OutcomePended, d.Outcome, "first inquiry still pended (PendFlips=2)")
	raw, err = e.Inquire(ref)
	require.NoError(t, err)
	d, _ = pas.Interpret(raw, now)
	require.Equal(t, workflow.OutcomeApproved, d.Outcome)
	require.NotEmpty(t, d.AuthNumber)

	_, err = e.Inquire("nope")
	require.ErrorIs(t, err, payersim.ErrUnknownRef)

	st := e.Stats()
	require.Equal(t, 1, st["approved"])
	require.Equal(t, 1, st["denied"])
	require.Equal(t, 0, st["pended"])

	require.Error(t, e.SetMode("weird"))
	require.NoError(t, e.SetMode(payersim.ModeRandom))
	seen := map[workflow.PayerOutcome]bool{}
	for range 40 {
		raw, err := e.Submit(bundle(1, 1), "")
		require.NoError(t, err)
		d, _ := pas.Interpret(raw, now)
		seen[d.Outcome] = true
	}
	require.Len(t, seen, 3, "seeded random covers every outcome")
}

func TestEngineRejectsBadBundles(t *testing.T) {
	e, _ := payersim.New(payersim.DefaultConfig(), nil)
	for _, b := range []string{`{`, `{"resourceType":"Patient"}`, `{"resourceType":"Bundle","entry":[]}`, `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`} {
		_, err := e.Submit([]byte(b), "")
		require.Error(t, err, b)
	}
	_, err := payersim.New(payersim.Config{Mode: "x"}, nil)
	require.Error(t, err)
	_, err = payersim.New(payersim.Config{Mode: payersim.ModeSmart, ErrorRate: 2}, nil)
	require.Error(t, err)
}

// The HTTP handler and the HTTP gateway are tested together: the gateway
// must be able to drive the simulator exactly as it would a real payer.
func TestHTTPHandlerWithGateway(t *testing.T) {
	cfg := payersim.DefaultConfig()
	cfg.Latency = 5 * time.Millisecond
	e, err := payersim.New(cfg, func() time.Time { return now })
	require.NoError(t, err)
	srv := httptest.NewServer(e.Handler())
	defer srv.Close()

	gw, err := payerhttp.New(srv.URL, 5*time.Second)
	require.NoError(t, err)
	gw.Headers = map[string]string{"Authorization": "Bearer x"}
	ctx := context.Background()
	require.NoError(t, gw.Ping(ctx))

	raw, err := gw.Submit(ctx, bundle(2, 1))
	require.NoError(t, err)
	d, err := pas.Interpret(raw, now)
	require.NoError(t, err)
	require.Equal(t, workflow.OutcomePended, d.Outcome)

	raw, err = gw.Inquire(ctx, d.PayerRef)
	require.NoError(t, err)
	d, err = pas.Interpret(raw, now)
	require.NoError(t, err)
	require.Equal(t, workflow.OutcomeApproved, d.Outcome)

	// 404 on unknown ref is a rejection (not retried).
	_, err = gw.Inquire(ctx, "nope")
	require.ErrorIs(t, err, payerhttp.ErrPayerRejected)

	// Bad bundle → 400 → rejected.
	_, err = gw.Submit(ctx, []byte(`{}`))
	require.ErrorIs(t, err, payerhttp.ErrPayerRejected)

	// Header override.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/Claim/$submit", strings.NewReader(string(bundle(9, 1))))
	req.Header.Set("X-Sim-Decision", "deny")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	var cr map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&cr))
	resp.Body.Close()
	require.Equal(t, "application/fhir+json", resp.Header.Get("Content-Type"))
	require.Contains(t, cr["disposition"], "Clinical criteria not met")

	// Admin endpoints.
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/admin/mode", strings.NewReader(`{"mode":"approve"}`))
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/admin/mode", strings.NewReader(`{"mode":"nope"}`))
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp, err = http.Get(srv.URL + "/admin/stats")
	require.NoError(t, err)
	var stats map[string]int
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&stats))
	resp.Body.Close()
	require.Equal(t, 2, stats["records"])
	resp, err = http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Empty body rejected.
	resp, err = http.Post(srv.URL+"/Claim/$submit", "application/fhir+json", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// Cancelled context aborts the latency sleep.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = payersim.Gateway{E: e}.Submit(cctx, bundle(1, 1))
	require.ErrorIs(t, err, context.Canceled)
}

func TestGatewayRetriesTransientFailures(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(pas.BuildResponse("P", "c", "Patient/p", workflow.OutcomeApproved, "A", nil, now))
	}))
	defer srv.Close()
	gw, err := payerhttp.New(srv.URL, time.Second)
	require.NoError(t, err)
	gw.Backoff = time.Millisecond
	raw, err := gw.Submit(context.Background(), bundle(1, 1))
	require.NoError(t, err)
	require.Equal(t, 3, calls)
	require.Contains(t, string(raw), "ClaimResponse")

	// Exhausted retries surface as unavailable.
	calls = -10
	_, err = gw.Submit(context.Background(), bundle(1, 1))
	require.ErrorIs(t, err, payerhttp.ErrPayerUnavailable)

	// Context cancellation during backoff.
	calls = -10
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer cancel()
	gw.Backoff = 50 * time.Millisecond
	_, err = gw.Submit(ctx, bundle(1, 1))
	require.Error(t, err)

	// Ping against a dead server is unavailable.
	srv.Close()
	require.ErrorIs(t, gw.Ping(context.Background()), payerhttp.ErrPayerUnavailable)

	_, err = payerhttp.New("not a url", time.Second)
	require.Error(t, err)
	_, err = payerhttp.New("", time.Second)
	require.Error(t, err)
}

func TestErrorInjection(t *testing.T) {
	cfg := payersim.DefaultConfig()
	cfg.ErrorRate = 1
	e, err := payersim.New(cfg, nil)
	require.NoError(t, err)
	_, err = payersim.Gateway{E: e}.Submit(context.Background(), bundle(1, 1))
	require.Error(t, err)
	srv := httptest.NewServer(e.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/Claim/$submit", "application/fhir+json", strings.NewReader(string(bundle(1, 1))))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, 2, e.Stats()["errors"])
}
