// Command payer-sim is a standalone simulated payer exposing PAS-style
// endpoints (POST /Claim/$submit, GET /ClaimResponse/{ref}) with
// configurable decision behaviour and latency.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/payersim"
	"github.com/udaykishore-resu/priorauth-engine/internal/observability"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	log := observability.NewLogger(env("SIM_LOG_LEVEL", "info"), os.Stderr)
	cfg := payersim.DefaultConfig()
	cfg.Mode = payersim.Mode(env("SIM_MODE", string(cfg.Mode)))
	var err error
	if cfg.Latency, err = time.ParseDuration(env("SIM_LATENCY", "0s")); err != nil {
		return fmt.Errorf("SIM_LATENCY: %w", err)
	}
	if cfg.PendFlips, err = strconv.Atoi(env("SIM_PEND_FLIPS", "1")); err != nil {
		return fmt.Errorf("SIM_PEND_FLIPS: %w", err)
	}
	if cfg.ErrorRate, err = strconv.ParseFloat(env("SIM_ERROR_RATE", "0"), 64); err != nil {
		return fmt.Errorf("SIM_ERROR_RATE: %w", err)
	}
	if v := os.Getenv("SIM_DENY_REASON"); v != "" {
		cfg.DenyReason = v
	}
	eng, err := payersim.New(cfg, nil)
	if err != nil {
		return err
	}

	addr := env("SIM_HTTP_ADDR", ":8081")
	srv := &http.Server{Addr: addr, Handler: eng.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		log.Info("payer-sim listening", "addr", addr, "mode", cfg.Mode, "latency", cfg.Latency, "error_rate", cfg.ErrorRate)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	log.Info("payer-sim stopped", "stats", eng.Stats())
	return nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
