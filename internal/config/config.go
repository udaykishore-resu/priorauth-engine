// Package config loads the 12-factor configuration from environment
// variables, applies defaults and validates the combination.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved service configuration.
type Config struct {
	ServiceName string
	Version     string
	HTTPAddr    string
	LogLevel    string

	ShutdownTimeout time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	MaxBodyBytes    int64

	RulesDir            string
	RulesReloadInterval time.Duration

	// Storage: "memory" or "postgres".
	Storage     string
	PostgresDSN string

	// Events: "memory" (record only) or "kafka".
	Events       string
	KafkaBrokers []string
	KafkaTopic   string

	// Payer: "sim" (in-process simulator) or "http".
	Payer        string
	PayerBaseURL string
	PayerTimeout time.Duration
	// Simulator behaviour when Payer == "sim".
	PayerSimMode    string
	PayerSimLatency time.Duration

	// LLM extractor (disabled when LLMBaseURL is empty).
	LLMBaseURL string
	LLMAPIKey  string
	LLMModel   string
	LLMTimeout time.Duration

	// SLA timers; zero disables.
	SLADetermine time.Duration
	SLAAssemble  time.Duration
	SLASubmit    time.Duration
	SLAResponse  time.Duration
	SLAPended    time.Duration
	SLAReview    time.Duration
	SLAUrgent    float64
	SLATick      time.Duration
	PollInterval time.Duration

	OTLPEndpoint string
	OTLPInsecure bool
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	var errs []error
	g := getter{errs: &errs}
	c := Config{
		ServiceName:         "priorauth-engine",
		Version:             g.str("PA_VERSION", "dev"),
		HTTPAddr:            g.str("PA_HTTP_ADDR", ":8080"),
		LogLevel:            g.str("PA_LOG_LEVEL", "info"),
		ShutdownTimeout:     g.dur("PA_SHUTDOWN_TIMEOUT", 20*time.Second),
		ReadTimeout:         g.dur("PA_HTTP_READ_TIMEOUT", 15*time.Second),
		WriteTimeout:        g.dur("PA_HTTP_WRITE_TIMEOUT", 60*time.Second),
		MaxBodyBytes:        g.i64("PA_HTTP_MAX_BODY_BYTES", 8<<20),
		RulesDir:            g.str("PA_RULES_DIR", "./rules"),
		RulesReloadInterval: g.dur("PA_RULES_RELOAD_INTERVAL", 5*time.Second),
		Storage:             strings.ToLower(g.str("PA_STORAGE", "memory")),
		PostgresDSN:         g.str("PA_POSTGRES_DSN", ""),
		Events:              strings.ToLower(g.str("PA_EVENTS", "memory")),
		KafkaBrokers:        g.list("PA_KAFKA_BROKERS", "localhost:9092"),
		KafkaTopic:          g.str("PA_KAFKA_TOPIC", "priorauth.events"),
		Payer:               strings.ToLower(g.str("PA_PAYER", "sim")),
		PayerBaseURL:        g.str("PA_PAYER_BASE_URL", "http://localhost:8081"),
		PayerTimeout:        g.dur("PA_PAYER_TIMEOUT", 10*time.Second),
		PayerSimMode:        g.str("PA_PAYER_SIM_MODE", "smart"),
		PayerSimLatency:     g.dur("PA_PAYER_SIM_LATENCY", 0),
		LLMBaseURL:          g.str("PA_LLM_BASE_URL", ""),
		LLMAPIKey:           g.str("PA_LLM_API_KEY", ""),
		LLMModel:            g.str("PA_LLM_MODEL", "gpt-4o-mini"),
		LLMTimeout:          g.dur("PA_LLM_TIMEOUT", 30*time.Second),
		SLADetermine:        g.dur("PA_SLA_DETERMINE", 4*time.Hour),
		SLAAssemble:         g.dur("PA_SLA_ASSEMBLE", 24*time.Hour),
		SLASubmit:           g.dur("PA_SLA_SUBMIT", 24*time.Hour),
		SLAResponse:         g.dur("PA_SLA_RESPONSE", 72*time.Hour),
		SLAPended:           g.dur("PA_SLA_PENDED", 7*24*time.Hour),
		SLAReview:           g.dur("PA_SLA_REVIEW", 48*time.Hour),
		SLAUrgent:           g.f64("PA_SLA_URGENT_FACTOR", 0.25),
		SLATick:             g.dur("PA_SLA_TICK", time.Minute),
		PollInterval:        g.dur("PA_PAYER_POLL_INTERVAL", 30*time.Second),
		OTLPEndpoint:        g.str("PA_OTLP_ENDPOINT", ""),
		OTLPInsecure:        g.boolean("PA_OTLP_INSECURE", true),
	}
	if err := c.Validate(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return c, nil
}

// Validate checks cross-field constraints.
func (c Config) Validate() error {
	var errs []error
	switch c.Storage {
	case "memory":
	case "postgres":
		if c.PostgresDSN == "" {
			errs = append(errs, errors.New("PA_POSTGRES_DSN is required when PA_STORAGE=postgres"))
		}
	default:
		errs = append(errs, fmt.Errorf("PA_STORAGE must be memory|postgres, got %q", c.Storage))
	}
	switch c.Events {
	case "memory":
	case "kafka":
		if len(c.KafkaBrokers) == 0 {
			errs = append(errs, errors.New("PA_KAFKA_BROKERS is required when PA_EVENTS=kafka"))
		}
	default:
		errs = append(errs, fmt.Errorf("PA_EVENTS must be memory|kafka, got %q", c.Events))
	}
	switch c.Payer {
	case "sim":
	case "http":
		if c.PayerBaseURL == "" {
			errs = append(errs, errors.New("PA_PAYER_BASE_URL is required when PA_PAYER=http"))
		}
	default:
		errs = append(errs, fmt.Errorf("PA_PAYER must be sim|http, got %q", c.Payer))
	}
	if c.RulesDir == "" {
		errs = append(errs, errors.New("PA_RULES_DIR must not be empty"))
	}
	if c.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("PA_HTTP_MAX_BODY_BYTES must be > 0"))
	}
	if c.SLAUrgent < 0 || c.SLAUrgent > 1 {
		errs = append(errs, errors.New("PA_SLA_URGENT_FACTOR must be within 0..1"))
	}
	if c.SLATick <= 0 || c.PollInterval <= 0 || c.RulesReloadInterval <= 0 {
		errs = append(errs, errors.New("intervals must be > 0"))
	}
	return errors.Join(errs...)
}

type getter struct{ errs *[]error }

func (g getter) str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func (g getter) dur(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*g.errs = append(*g.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}

func (g getter) i64(key string, def int64) int64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		*g.errs = append(*g.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return n
}

func (g getter) f64(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		*g.errs = append(*g.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return f
}

func (g getter) boolean(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*g.errs = append(*g.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return b
}

func (g getter) list(key, def string) []string {
	raw := g.str(key, def)
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
