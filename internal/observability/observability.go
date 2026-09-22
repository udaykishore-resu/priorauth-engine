// Package observability wires structured logging, OpenTelemetry tracing and
// Prometheus metrics. Everything here is optional at runtime: with no OTLP
// endpoint configured, tracing is a no-op and the process still exposes
// /metrics.
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// NewLogger returns a JSON slog logger at the given level ("debug", "info",
// "warn", "error"). Output defaults to stderr.
func NewLogger(level string, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl}))
}

// SetupTracing installs a global tracer provider. When endpoint is empty a
// provider with no exporter is installed so spans are cheap no-ops that
// still propagate context. The returned function flushes and shuts down.
func SetupTracing(ctx context.Context, serviceName, version, endpoint string, insecure bool) (func(context.Context) error, error) {
	// Schemaless so the merge never conflicts with the SDK default resource's
	// schema URL as the semconv package version moves.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, fmt.Errorf("observability: resource: %w", err)
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if endpoint != "" {
		expOpts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}
		if insecure {
			expOpts = append(expOpts, otlptracehttp.WithInsecure())
		}
		exp, err := otlptracehttp.New(ctx, expOpts...)
		if err != nil {
			return nil, fmt.Errorf("observability: otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(2*time.Second)))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp.Shutdown, nil
}

// Tracer returns the service tracer.
func Tracer() trace.Tracer { return otel.Tracer("priorauth-engine") }

// TraceID extracts the current trace id for log correlation ("" if none).
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// Metrics holds every Prometheus series the service emits.
type Metrics struct {
	Registry *prometheus.Registry

	// RED for the HTTP API.
	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec
	HTTPInFlight prometheus.Gauge

	// Domain.
	Determinations  *prometheus.CounterVec // decision, criteria, rule
	Submissions     *prometheus.CounterVec // result
	PayerDecisions  *prometheus.CounterVec // outcome
	Transitions     *prometheus.CounterVec // to_state
	ExceptionsOpen  *prometheus.GaugeVec   // code
	ExceptionsTotal *prometheus.CounterVec // code
	SLABreaches     *prometheus.CounterVec // timer
	Turnaround      *prometheus.HistogramVec
	Proposals       *prometheus.CounterVec // corroborated
	RulesLoaded     prometheus.Gauge
	RuleReloads     prometheus.Counter
	PayerLatency    *prometheus.HistogramVec // op
	EventsPublished *prometheus.CounterVec   // result
}

// NewMetrics registers all series on a fresh registry (plus Go/process
// collectors), so tests can create many instances without collisions.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := promauto{reg: reg, ns: "priorauth"}
	m := &Metrics{
		Registry:     reg,
		HTTPRequests: f.counter("http_requests_total", "HTTP requests by route, method and status.", "route", "method", "status"),
		HTTPDuration: f.histogram("http_request_duration_seconds", "HTTP request latency.", []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}, "route", "method"),
		HTTPInFlight: f.gauge("http_in_flight_requests", "HTTP requests currently being served."),

		Determinations:  f.counter("determinations_total", "Rule determinations by decision, criteria outcome and rule.", "decision", "criteria", "rule"),
		Submissions:     f.counter("submissions_total", "PAS submissions by result.", "result"),
		PayerDecisions:  f.counter("payer_decisions_total", "Interpreted payer decisions.", "outcome"),
		Transitions:     f.counter("state_transitions_total", "Request state transitions.", "to_state"),
		ExceptionsOpen:  f.gaugeVec("exceptions_open", "Open exceptions on the work queue.", "code"),
		ExceptionsTotal: f.counter("exceptions_total", "Exceptions raised.", "code"),
		SLABreaches:     f.counter("sla_breaches_total", "SLA timer breaches.", "timer"),
		Turnaround:      f.histogram("turnaround_seconds", "Created → final decision.", []float64{60, 300, 900, 3600, 4 * 3600, 24 * 3600, 3 * 24 * 3600, 7 * 24 * 3600}, "payer", "outcome"),
		Proposals:       f.counter("llm_proposals_total", "LLM evidence proposals by corroboration.", "corroborated"),
		RulesLoaded:     f.gauge("rules_loaded", "Rules in the active set."),
		RuleReloads:     f.counterPlain("rule_reloads_total", "Successful hot reloads."),
		PayerLatency:    f.histogram("payer_gateway_duration_seconds", "Payer gateway latency.", []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30}, "op", "result"),
		EventsPublished: f.counter("events_published_total", "Domain events published downstream.", "result"),
	}
	return m
}

// promauto registers series on a single registry under one namespace.
type promauto struct {
	reg *prometheus.Registry
	ns  string
}

func (p promauto) counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: p.ns, Name: name, Help: help}, labels)
	p.reg.MustRegister(c)
	return c
}

func (p promauto) counterPlain(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Namespace: p.ns, Name: name, Help: help})
	p.reg.MustRegister(c)
	return c
}

func (p promauto) gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: p.ns, Name: name, Help: help})
	p.reg.MustRegister(g)
	return g
}

func (p promauto) gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: p.ns, Name: name, Help: help}, labels)
	p.reg.MustRegister(g)
	return g
}

func (p promauto) histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: p.ns, Name: name, Help: help, Buckets: buckets}, labels)
	p.reg.MustRegister(h)
	return h
}
