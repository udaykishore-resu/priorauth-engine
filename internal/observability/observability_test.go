package observability

import (
	"bytes"
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestLoggerLevels(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger("warn", &buf)
	log.Info("hidden")
	log.Warn("shown", "k", "v")
	require.NotContains(t, buf.String(), "hidden")
	require.Contains(t, buf.String(), `"msg":"shown"`)
	require.Contains(t, buf.String(), `"k":"v"`)
	for _, lvl := range []string{"debug", "info", "error", "warning", "nonsense"} {
		require.NotNil(t, NewLogger(lvl, nil))
	}
}

func TestTracingNoop(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), "svc", "v", "", true)
	require.NoError(t, err)
	ctx, span := Tracer().Start(context.Background(), "op")
	require.NotEmpty(t, TraceID(ctx), "sdk provider always assigns trace ids")
	span.End()
	require.Empty(t, TraceID(context.Background()))
	require.NoError(t, shutdown(context.Background()))

	shutdown, err = SetupTracing(context.Background(), "svc", "v", "http://127.0.0.1:1", true)
	require.NoError(t, err, "exporter creation is lazy; no connection yet")
	_ = shutdown(context.Background())
}

func TestMetricsRegistered(t *testing.T) {
	m := NewMetrics()
	m.Determinations.WithLabelValues("required", "met", "r@1").Inc()
	m.RulesLoaded.Set(5)
	require.Equal(t, float64(5), testutil.ToFloat64(m.RulesLoaded))
	n, err := testutil.GatherAndCount(m.Registry, "priorauth_determinations_total", "priorauth_rules_loaded")
	require.NoError(t, err)
	require.Equal(t, 2, n)
	// A second instance must not collide (fresh registry).
	require.NotNil(t, NewMetrics())
}
