package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/memory"
	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/payersim"
	"github.com/udaykishore-resu/priorauth-engine/internal/app"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/rules"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
	"github.com/udaykishore-resu/priorauth-engine/internal/observability"
)

type flakyGateway struct {
	inner payersim.Gateway
	fail  bool
}

func (g *flakyGateway) Submit(ctx context.Context, b []byte) ([]byte, error) {
	if g.fail {
		return nil, errors.New("boom")
	}
	return g.inner.Submit(ctx, b)
}
func (g *flakyGateway) Inquire(ctx context.Context, ref string) ([]byte, error) {
	return g.inner.Inquire(ctx, ref)
}
func (g *flakyGateway) Ping(context.Context) error {
	if g.fail {
		return errors.New("boom")
	}
	return nil
}

func newTestServer(t *testing.T) (*httptest.Server, *Server, *flakyGateway) {
	t.Helper()
	set, err := rules.DirLoader{Dir: "../../../rules"}.Load()
	require.NoError(t, err)
	sim, err := payersim.New(payersim.DefaultConfig(), nil)
	require.NoError(t, err)
	gw := &flakyGateway{inner: payersim.Gateway{E: sim}}
	repo := memory.NewRepository()
	metrics := observability.NewMetrics()
	svc, err := app.New(app.Deps{
		Repo: repo, Rules: rules.NewStore(set), Gateway: gw, Structured: evidence.FHIRExtractor{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: metrics,
	})
	require.NoError(t, err)
	s := New(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics, Readiness{Repo: repo, Gateway: gw}, 1<<20, "test")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s, gw
}

func do(t *testing.T, ts *httptest.Server, method, path string, body []byte, hdr ...string) (int, map[string]any, http.Header) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		require.NoError(t, json.Unmarshal(raw, &out), string(raw))
	}
	return resp.StatusCode, out, resp.Header
}

func example(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../examples/" + name)
	require.NoError(t, err)
	return b
}

func TestEndToEndOverHTTP(t *testing.T) {
	ts, _, _ := newTestServer(t)

	status, body, hdr := do(t, ts, http.MethodPost, "/v1/requests", example(t, "01-mri-lumbar-spine.json"))
	require.Equal(t, http.StatusCreated, status, body)
	id := body["id"].(string)
	require.Equal(t, "/v1/requests/"+id, hdr.Get("Location"))
	require.NotEmpty(t, hdr.Get("X-Request-ID"))

	status, body, hdr = do(t, ts, http.MethodPost, "/v1/requests", example(t, "01-mri-lumbar-spine.json"))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, id, body["id"])
	require.Equal(t, "true", hdr.Get("Idempotent-Replay"))

	// Idempotency-Key header takes precedence over the body key.
	status, body, _ = do(t, ts, http.MethodPost, "/v1/requests", example(t, "01-mri-lumbar-spine.json"), "Idempotency-Key", "other-key")
	require.Equal(t, http.StatusCreated, status)
	require.NotEqual(t, id, body["id"])

	status, body, _ = do(t, ts, http.MethodPost, "/v1/requests/"+id+"/submit", nil)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "invalid_transition", body["code"])

	status, body, _ = do(t, ts, http.MethodPost, "/v1/requests/"+id+"/determine", nil)
	require.Equal(t, http.StatusOK, status)
	det := body["determination"].(map[string]any)
	require.Equal(t, "required", det["decision"])
	require.Equal(t, "met", det["criteria"])
	require.NotNil(t, det["explain"])

	status, body, _ = do(t, ts, http.MethodPost, "/v1/requests/"+id+"/assemble", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, true, body["evidence"].(map[string]any)["complete"])

	status, body, _ = do(t, ts, http.MethodPost, "/v1/requests/"+id+"/submit", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "approved", body["state"])

	status, body, _ = do(t, ts, http.MethodGet, "/v1/requests/"+id, nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "approved", body["state"])

	status, body, _ = do(t, ts, http.MethodGet, "/v1/requests/"+id+"/events", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, float64(5), body["count"])

	status, body, _ = do(t, ts, http.MethodGet, "/v1/requests?state=approved&limit=10", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, float64(1), body["count"])

	status, body, _ = do(t, ts, http.MethodGet, "/v1/queue/exceptions", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, float64(0), body["count"])

	status, body, _ = do(t, ts, http.MethodGet, "/v1/rules", nil)
	require.Equal(t, http.StatusOK, status)
	require.GreaterOrEqual(t, len(body["rules"].([]any)), 5)

	status, body, _ = do(t, ts, http.MethodGet, "/v1/metrics/turnaround", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, float64(1), body["decided"].(map[string]any)["count"])

	// Prometheus exposition contains RED and domain series.
	resp, err := ts.Client().Get(ts.URL + "/metrics")
	require.NoError(t, err)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Contains(t, string(raw), `priorauth_http_requests_total{method="POST",route="/v1/requests/{id}/submit",status="200"}`)
	require.Contains(t, string(raw), `priorauth_determinations_total{criteria="met",decision="required",rule="acme.imaging.mri-lumbar-spine@3"}`)
	require.Contains(t, string(raw), `priorauth_state_transitions_total{to_state="approved"}`)
}

func TestReviewAndExceptionsOverHTTP(t *testing.T) {
	ts, _, gw := newTestServer(t)
	_, body, _ := do(t, ts, http.MethodPost, "/v1/requests", example(t, "04-mri-needs-review.json"))
	id := body["id"].(string)
	do(t, ts, http.MethodPost, "/v1/requests/"+id+"/determine", nil)
	do(t, ts, http.MethodPost, "/v1/requests/"+id+"/assemble", nil)

	status, body, _ := do(t, ts, http.MethodGet, "/v1/queue/exceptions", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, float64(1), body["count"])
	item := body["items"].([]any)[0].(map[string]any)
	require.Equal(t, id, item["request_id"])
	require.Equal(t, "criteria_unknown", item["exception"].(map[string]any)["code"])

	status, body, _ = do(t, ts, http.MethodPost, "/v1/requests/"+id+"/review", []byte(`{"action":"override_criteria","actor":"md"}`))
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "validation", body["code"])

	status, _, _ = do(t, ts, http.MethodPost, "/v1/requests/"+id+"/review", example(t, "review-override.json"))
	require.Equal(t, http.StatusOK, status)
	status, body, _ = do(t, ts, http.MethodPost, "/v1/requests/"+id+"/assemble", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, true, body["evidence"].(map[string]any)["complete"])

	// Payer down → 502 with the persisted request (exception recorded).
	gw.fail = true
	status, body, _ = do(t, ts, http.MethodPost, "/v1/requests/"+id+"/submit", nil)
	require.Equal(t, http.StatusBadGateway, status)
	require.Equal(t, "payer_unavailable", body["code"])
	require.Equal(t, "assembled", body["request"].(map[string]any)["state"])
	status, body, _ = do(t, ts, http.MethodGet, "/readyz", nil)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "not_ready", body["status"])
	gw.fail = false

	status, body, _ = do(t, ts, http.MethodPost, "/v1/requests/"+id+"/submit", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "pended", body["state"], "thin evidence → smart sim pends")

	status, body, _ = do(t, ts, http.MethodPost, "/v1/requests/"+id+"/sync", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "approved", body["state"])
}

func TestErrorMapping(t *testing.T) {
	ts, s, _ := newTestServer(t)
	tests := []struct {
		name   string
		method string
		path   string
		body   []byte
		hdr    []string
		status int
		code   string
	}{
		{"not found", http.MethodGet, "/v1/requests/nope", nil, nil, http.StatusNotFound, "not_found"},
		{"not found command", http.MethodPost, "/v1/requests/nope/determine", nil, nil, http.StatusNotFound, "not_found"},
		{"invalid json", http.MethodPost, "/v1/requests", []byte(`{`), nil, http.StatusBadRequest, "validation"},
		{"validation", http.MethodPost, "/v1/requests", []byte(`{"patient":{}}`), nil, http.StatusBadRequest, "validation"},
		{"wrong content type", http.MethodPost, "/v1/requests", []byte(`{}`), []string{"Content-Type", "text/plain"}, http.StatusBadRequest, "validation"},
		{"bad limit", http.MethodGet, "/v1/requests?limit=0", nil, nil, http.StatusBadRequest, "validation"},
		{"unknown route", http.MethodGet, "/v1/nothing", nil, nil, http.StatusNotFound, ""},
		{"method not allowed", http.MethodDelete, "/v1/requests", nil, nil, http.StatusMethodNotAllowed, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, body, _ := do(t, ts, tc.method, tc.path, tc.body, tc.hdr...)
			require.Equal(t, tc.status, status)
			if tc.code != "" {
				require.Equal(t, tc.code, body["code"])
			}
		})
	}

	t.Run("body too large", func(t *testing.T) {
		big := []byte(`{"pad":"` + strings.Repeat("x", 2<<20) + `"}`)
		status, body, _ := do(t, ts, http.MethodPost, "/v1/requests", big)
		require.Equal(t, http.StatusRequestEntityTooLarge, status)
		require.Equal(t, "body_too_large", body["code"])
	})

	t.Run("health and draining", func(t *testing.T) {
		status, body, _ := do(t, ts, http.MethodGet, "/healthz", nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "test", body["version"])
		status, body, _ = do(t, ts, http.MethodGet, "/readyz", nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "ready", body["status"])
		s.Draining()
		s.Draining() // idempotent
		status, body, _ = do(t, ts, http.MethodGet, "/readyz", nil)
		require.Equal(t, http.StatusServiceUnavailable, status)
		require.Equal(t, "draining", body["status"])
	})
}

func TestWriteErrorMapsInfrastructureErrors(t *testing.T) {
	_, s, _ := newTestServer(t)
	cases := map[error]int{
		errors.New("unexpected"):           http.StatusInternalServerError,
		context.DeadlineExceeded:           http.StatusGatewayTimeout,
		workflow.ErrInvalidTransition:      http.StatusConflict,
		app.ErrPayerUnavailable:            http.StatusBadGateway,
		workflow.ErrValidation:             http.StatusBadRequest,
		workflow.ErrNotFound:               http.StatusNotFound,
		errors.Join(concurrencyErr(), nil): http.StatusConflict,
	}
	for err, want := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		s.writeError(rec, req, err)
		require.Equal(t, want, rec.Code, err.Error())
	}
}

func concurrencyErr() error {
	repo := memory.NewRepository()
	r := &workflow.Request{ID: "r", Version: 5}
	return repo.Save(context.Background(), r, []workflow.Envelope{{RequestID: "r", Seq: 5}})
}

func TestPanicRecovery(t *testing.T) {
	_, s, _ := newTestServer(t)
	h := s.instrument(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("kaboom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/requests/abc", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "/v1/requests/{id}", routeLabel(httptest.NewRequest(http.MethodGet, "/v1/requests/abc", nil)))
	require.Equal(t, "/v1/requests/{id}/submit", routeLabel(httptest.NewRequest(http.MethodPost, "/v1/requests/abc/submit", nil)))
	require.Equal(t, "/healthz", routeLabel(httptest.NewRequest(http.MethodGet, "/healthz", nil)))
}
