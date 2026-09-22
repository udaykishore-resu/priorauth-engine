package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	c, err := Load()
	require.NoError(t, err)
	require.Equal(t, ":8080", c.HTTPAddr)
	require.Equal(t, "memory", c.Storage)
	require.Equal(t, "sim", c.Payer)
	require.Equal(t, 4*time.Hour, c.SLADetermine)
	require.Equal(t, []string{"localhost:9092"}, c.KafkaBrokers)
}

func TestLoadOverridesAndErrors(t *testing.T) {
	t.Setenv("PA_HTTP_ADDR", ":18499")
	t.Setenv("PA_SLA_PENDED", "36h")
	t.Setenv("PA_KAFKA_BROKERS", "a:1, b:2,")
	t.Setenv("PA_OTLP_INSECURE", "false")
	t.Setenv("PA_HTTP_MAX_BODY_BYTES", "1024")
	t.Setenv("PA_SLA_URGENT_FACTOR", "0.5")
	c, err := Load()
	require.NoError(t, err)
	require.Equal(t, ":18499", c.HTTPAddr)
	require.Equal(t, 36*time.Hour, c.SLAPended)
	require.Equal(t, []string{"a:1", "b:2"}, c.KafkaBrokers)
	require.False(t, c.OTLPInsecure)
	require.Equal(t, int64(1024), c.MaxBodyBytes)

	t.Setenv("PA_SLA_PENDED", "soon")
	_, err = Load()
	require.ErrorContains(t, err, "PA_SLA_PENDED")
	t.Setenv("PA_SLA_PENDED", "1h")
	t.Setenv("PA_HTTP_MAX_BODY_BYTES", "x")
	_, err = Load()
	require.ErrorContains(t, err, "PA_HTTP_MAX_BODY_BYTES")
	t.Setenv("PA_HTTP_MAX_BODY_BYTES", "1")
	t.Setenv("PA_SLA_URGENT_FACTOR", "abc")
	_, err = Load()
	require.ErrorContains(t, err, "PA_SLA_URGENT_FACTOR")
	t.Setenv("PA_SLA_URGENT_FACTOR", "0.5")
	t.Setenv("PA_OTLP_INSECURE", "maybe")
	_, err = Load()
	require.ErrorContains(t, err, "PA_OTLP_INSECURE")
}

func TestValidate(t *testing.T) {
	base := func() Config {
		c, _ := Load()
		return c
	}
	tests := []struct {
		name string
		mut  func(*Config)
		err  string
	}{
		{"postgres needs dsn", func(c *Config) { c.Storage = "postgres" }, "PA_POSTGRES_DSN"},
		{"bad storage", func(c *Config) { c.Storage = "disk" }, "PA_STORAGE"},
		{"kafka needs brokers", func(c *Config) { c.Events = "kafka"; c.KafkaBrokers = nil }, "PA_KAFKA_BROKERS"},
		{"bad events", func(c *Config) { c.Events = "pigeon" }, "PA_EVENTS"},
		{"http payer needs url", func(c *Config) { c.Payer = "http"; c.PayerBaseURL = "" }, "PA_PAYER_BASE_URL"},
		{"bad payer", func(c *Config) { c.Payer = "fax" }, "PA_PAYER"},
		{"rules dir", func(c *Config) { c.RulesDir = "" }, "PA_RULES_DIR"},
		{"body bytes", func(c *Config) { c.MaxBodyBytes = 0 }, "PA_HTTP_MAX_BODY_BYTES"},
		{"urgent factor", func(c *Config) { c.SLAUrgent = 2 }, "PA_SLA_URGENT_FACTOR"},
		{"intervals", func(c *Config) { c.SLATick = 0 }, "intervals"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mut(&c)
			require.ErrorContains(t, c.Validate(), tc.err)
		})
	}
}
