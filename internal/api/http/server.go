// Package httpapi exposes the application over net/http using Go 1.22+
// ServeMux method/path patterns. It owns request decoding, error mapping,
// RED metrics, request-id/trace-id logging and the health endpoints.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/udaykishore-resu/priorauth-engine/internal/app"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
	"github.com/udaykishore-resu/priorauth-engine/internal/observability"
	"github.com/udaykishore-resu/priorauth-engine/internal/ports"
)

// Readiness dependencies checked by /readyz.
type Readiness struct {
	Repo    ports.Repository
	Gateway ports.PayerGateway
}

// Server bundles the handler dependencies.
type Server struct {
	svc      *app.Service
	log      *slog.Logger
	metrics  *observability.Metrics
	ready    Readiness
	maxBody  int64
	version  string
	tracer   trace.Tracer
	shutdown chan struct{}
}

// New constructs the server.
func New(svc *app.Service, log *slog.Logger, metrics *observability.Metrics, ready Readiness, maxBody int64, version string) *Server {
	if maxBody <= 0 {
		maxBody = 8 << 20
	}
	return &Server{svc: svc, log: log, metrics: metrics, ready: ready, maxBody: maxBody, version: version, tracer: observability.Tracer(), shutdown: make(chan struct{})}
}

// Draining flips /readyz to 503 so load balancers stop sending traffic
// before the listener closes.
func (s *Server) Draining() {
	select {
	case <-s.shutdown:
	default:
		close(s.shutdown)
	}
}

// Handler builds the routed, instrumented handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{}))

	mux.HandleFunc("POST /v1/requests", s.createRequest)
	mux.HandleFunc("GET /v1/requests", s.listRequests)
	mux.HandleFunc("GET /v1/requests/{id}", s.getRequest)
	mux.HandleFunc("GET /v1/requests/{id}/events", s.getEvents)
	mux.HandleFunc("POST /v1/requests/{id}/determine", s.command(s.svc.Determine))
	mux.HandleFunc("POST /v1/requests/{id}/assemble", s.command(s.svc.Assemble))
	mux.HandleFunc("POST /v1/requests/{id}/submit", s.command(s.svc.Submit))
	mux.HandleFunc("POST /v1/requests/{id}/sync", s.command(s.svc.Sync))
	mux.HandleFunc("POST /v1/requests/{id}/review", s.review)
	mux.HandleFunc("GET /v1/queue/exceptions", s.exceptions)
	mux.HandleFunc("GET /v1/rules", s.rules)
	mux.HandleFunc("GET /v1/metrics/turnaround", s.turnaround)

	return s.instrument(mux)
}

// ---------------------------------------------------------------------------
// Handlers.

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	select {
	case <-s.shutdown:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	default:
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]string{}
	status := http.StatusOK
	check := func(name string, fn func(context.Context) error) {
		if fn == nil {
			return
		}
		if err := fn(ctx); err != nil {
			checks[name] = "fail: " + err.Error()
			status = http.StatusServiceUnavailable
			return
		}
		checks[name] = "ok"
	}
	if s.ready.Repo != nil {
		check("repository", s.ready.Repo.Ping)
	}
	if s.ready.Gateway != nil {
		check("payer_gateway", s.ready.Gateway.Ping)
	}
	checks["rules"] = fmt.Sprintf("ok (%d rules, set %s)", len(s.svc.Rules().Rules), s.svc.Rules().Hash)
	writeJSON(w, status, map[string]any{"status": map[bool]string{true: "ready", false: "not_ready"}[status == http.StatusOK], "checks": checks})
}

func (s *Server) createRequest(w http.ResponseWriter, r *http.Request) {
	var in workflow.Intake
	if !s.decode(w, r, &in) {
		return
	}
	if k := strings.TrimSpace(r.Header.Get("Idempotency-Key")); k != "" {
		in.IdempotencyKey = k
	}
	req, created, err := s.svc.Create(r.Context(), in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/requests/"+req.ID)
	if created {
		writeJSON(w, http.StatusCreated, req)
		return
	}
	w.Header().Set("Idempotent-Replay", "true")
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ports.Filter{State: workflow.State(q.Get("state")), Payer: q.Get("payer"), Active: q.Get("active") == "true", Limit: 100}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n <= 0 || n > 1000 {
			s.writeError(w, r, fmt.Errorf("%w: limit must be 1..1000", workflow.ErrValidation))
			return
		}
		f.Limit = n
	}
	list, err := s.svc.List(r.Context(), f)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if list == nil {
		list = []*workflow.Request{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "count": len(list)})
}

func (s *Server) getRequest(w http.ResponseWriter, r *http.Request) {
	req, err := s.svc.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) getEvents(w http.ResponseWriter, r *http.Request) {
	evs, err := s.svc.Events(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request_id": r.PathValue("id"), "events": evs, "count": len(evs)})
}

// command adapts a (ctx, id) → (*Request, error) use case to a handler.
func (s *Server) command(fn func(context.Context, string) (*workflow.Request, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := fn(r.Context(), r.PathValue("id"))
		if err != nil {
			if errors.Is(err, app.ErrPayerUnavailable) && req != nil {
				// The request is persisted with an exception; tell the client
				// both what failed and where the request now stands.
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "code": "payer_unavailable", "request": req})
				return
			}
			s.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, req)
	}
}

func (s *Server) review(w http.ResponseWriter, r *http.Request) {
	var rv workflow.Review
	if !s.decode(w, r, &rv) {
		return
	}
	req, err := s.svc.Review(r.Context(), r.PathValue("id"), rv)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) exceptions(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.Exceptions(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if items == nil {
		items = []app.ExceptionItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

func (s *Server) rules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Rules())
}

func (s *Server) turnaround(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.Turnaround(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ---------------------------------------------------------------------------
// Plumbing.

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") && !strings.HasPrefix(ct, "application/fhir+json") {
		s.writeError(w, r, fmt.Errorf("%w: Content-Type must be application/json", workflow.ErrValidation))
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errBody("body_too_large", err.Error()))
			return false
		}
		s.writeError(w, r, fmt.Errorf("%w: invalid JSON body: %w", workflow.ErrValidation, err))
		return false
	}
	return true
}

func errBody(code, msg string) map[string]string {
	return map[string]string{"code": code, "error": msg}
}

// writeError maps domain errors onto HTTP statuses. Unknown errors are 500
// and the detail is logged, not leaked.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, workflow.ErrValidation):
		writeJSON(w, http.StatusBadRequest, errBody("validation", err.Error()))
	case errors.Is(err, workflow.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errBody("not_found", err.Error()))
	case errors.Is(err, workflow.ErrInvalidTransition):
		writeJSON(w, http.StatusConflict, errBody("invalid_transition", err.Error()))
	case errors.Is(err, ports.ErrConcurrency):
		writeJSON(w, http.StatusConflict, errBody("concurrent_modification", "the request was modified concurrently; retry"))
	case errors.Is(err, app.ErrPayerUnavailable):
		writeJSON(w, http.StatusBadGateway, errBody("payer_unavailable", err.Error()))
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeJSON(w, http.StatusGatewayTimeout, errBody("timeout", "request timed out"))
	default:
		s.log.ErrorContext(r.Context(), "internal error", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("internal", "internal error"))
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (sw *statusWriter) WriteHeader(code int) {
	if sw.status == 0 {
		sw.status = code
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if sw.status == 0 {
		sw.status = http.StatusOK
	}
	n, err := sw.ResponseWriter.Write(b)
	sw.bytes += n
	return n, err
}

// instrument adds request id, tracing, RED metrics, access logging and
// panic recovery around the mux.
func (s *Server) instrument(next http.Handler) http.Handler {
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	otel.SetTextMapPropagator(prop)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", reqID)

		ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		route := routeLabel(r)
		ctx, span := s.tracer.Start(ctx, r.Method+" "+route, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		r = r.WithContext(ctx)

		sw := &statusWriter{ResponseWriter: w}
		s.metrics.HTTPInFlight.Inc()
		defer s.metrics.HTTPInFlight.Dec()
		defer func() {
			if rec := recover(); rec != nil {
				s.log.ErrorContext(ctx, "panic recovered", "panic", fmt.Sprint(rec), "path", r.URL.Path, "request_id", reqID)
				if sw.status == 0 {
					writeJSON(sw, http.StatusInternalServerError, errBody("internal", "internal error"))
				}
			}
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			dur := time.Since(start)
			s.metrics.HTTPRequests.WithLabelValues(route, r.Method, strconv.Itoa(status)).Inc()
			s.metrics.HTTPDuration.WithLabelValues(route, r.Method).Observe(dur.Seconds())
			if route != "/metrics" && route != "/healthz" && route != "/readyz" {
				s.log.InfoContext(ctx, "http",
					"method", r.Method, "path", r.URL.Path, "route", route, "status", status,
					"bytes", sw.bytes, "duration_ms", dur.Milliseconds(),
					"request_id", reqID, "trace_id", observability.TraceID(ctx), "remote", r.RemoteAddr)
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

// routeLabel collapses ids into a bounded-cardinality label.
func routeLabel(r *http.Request) string {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "requests" {
		parts[2] = "{id}"
	}
	return "/" + strings.Join(parts, "/")
}
