// Command priorauth runs the prior-authorization engine: config → adapters →
// domain → HTTP → background loops → graceful shutdown. Wiring only.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/kafka"
	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/llm"
	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/memory"
	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/payerhttp"
	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/payersim"
	"github.com/udaykishore-resu/priorauth-engine/internal/adapters/postgres"
	httpapi "github.com/udaykishore-resu/priorauth-engine/internal/api/http"
	"github.com/udaykishore-resu/priorauth-engine/internal/app"
	"github.com/udaykishore-resu/priorauth-engine/internal/config"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/rules"
	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
	"github.com/udaykishore-resu/priorauth-engine/internal/observability"
	"github.com/udaykishore-resu/priorauth-engine/internal/ports"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := observability.NewLogger(cfg.LogLevel, os.Stderr)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := observability.SetupTracing(ctx, cfg.ServiceName, cfg.Version, cfg.OTLPEndpoint, cfg.OTLPInsecure)
	if err != nil {
		return err
	}
	metrics := observability.NewMetrics()

	// Rules: load once, then hot-reload on file changes.
	loader := rules.DirLoader{Dir: cfg.RulesDir}
	set, err := loader.Load()
	if err != nil {
		return fmt.Errorf("load rules: %w", err)
	}
	store := rules.NewStore(set)
	log.Info("rules loaded", "dir", cfg.RulesDir, "count", len(set.Rules), "hash", set.Hash)

	// Persistence.
	var repo ports.Repository
	var closers []func() error
	switch cfg.Storage {
	case "postgres":
		pg, err := postgres.Open(ctx, cfg.PostgresDSN)
		if err != nil {
			return err
		}
		if err := pg.Migrate(ctx); err != nil {
			return err
		}
		repo = pg
		closers = append(closers, func() error { pg.Close(); return nil })
		log.Info("storage: postgres")
	default:
		repo = memory.NewRepository()
		log.Info("storage: in-memory (state is lost on restart)")
	}

	// Events.
	var pub ports.Publisher
	switch cfg.Events {
	case "kafka":
		kp, err := kafka.NewPublisher(cfg.KafkaBrokers, cfg.KafkaTopic)
		if err != nil {
			return err
		}
		pub = kp
		log.Info("events: kafka", "brokers", cfg.KafkaBrokers, "topic", cfg.KafkaTopic)
	default:
		pub = memory.NewPublisher()
		log.Info("events: in-memory")
	}
	closers = append(closers, pub.Close)

	// Payer.
	var gateway ports.PayerGateway
	switch cfg.Payer {
	case "http":
		gw, err := payerhttp.New(cfg.PayerBaseURL, cfg.PayerTimeout)
		if err != nil {
			return err
		}
		gateway = gw
		log.Info("payer: http", "base_url", cfg.PayerBaseURL)
	default:
		simCfg := payersim.DefaultConfig()
		simCfg.Mode = payersim.Mode(cfg.PayerSimMode)
		simCfg.Latency = cfg.PayerSimLatency
		eng, err := payersim.New(simCfg, nil)
		if err != nil {
			return err
		}
		gateway = payersim.Gateway{E: eng}
		log.Info("payer: in-process simulator", "mode", simCfg.Mode)
	}

	// Evidence extractors.
	var proposers []evidence.Extractor
	if cfg.LLMBaseURL != "" {
		proposers = append(proposers, llm.New(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.LLMTimeout))
		log.Info("llm extractor enabled", "base_url", cfg.LLMBaseURL, "model", cfg.LLMModel)
	} else {
		log.Info("llm extractor disabled (PA_LLM_BASE_URL empty)")
	}

	svc, err := app.New(app.Deps{
		Repo: repo, Publisher: pub, Rules: store, Gateway: gateway,
		Structured: evidence.FHIRExtractor{}, Proposers: proposers,
		SLA: workflow.SLAPolicy{
			Determine: cfg.SLADetermine, Assemble: cfg.SLAAssemble, Submit: cfg.SLASubmit,
			Response: cfg.SLAResponse, Pended: cfg.SLAPended, Review: cfg.SLAReview, UrgentFactor: cfg.SLAUrgent,
		},
		Logger: log, Metrics: metrics,
	})
	if err != nil {
		return err
	}

	api := httpapi.New(svc, log, metrics, httpapi.Readiness{Repo: repo, Gateway: gateway}, cfg.MaxBodyBytes, cfg.Version)
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Handler(),
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return context.Background() },
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		log.Info("http listening", "addr", cfg.HTTPAddr, "version", cfg.Version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		store.Watch(gctx, loader, cfg.RulesReloadInterval, log)
		return nil
	})
	g.Go(func() error {
		return loop(gctx, cfg.SLATick, func(ctx context.Context) {
			if n, err := svc.CheckSLAs(ctx); err != nil {
				log.Warn("sla check failed", "err", err)
			} else if n > 0 {
				log.Info("sla check", "breached_requests", n)
			}
		})
	})
	g.Go(func() error {
		return loop(gctx, cfg.PollInterval, func(ctx context.Context) {
			if n, err := svc.PollPended(ctx); err != nil {
				log.Warn("payer poll failed", "err", err)
			} else if n > 0 {
				log.Info("payer poll", "synced", n)
			}
		})
	})
	g.Go(func() error {
		<-gctx.Done()
		log.Info("shutdown: draining", "timeout", cfg.ShutdownTimeout.String())
		api.Draining()
		sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		return nil
	})
	err = g.Wait()

	for _, c := range closers {
		if cerr := c(); cerr != nil {
			log.Warn("close", "err", cerr)
		}
	}
	fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if terr := shutdownTracing(fctx); terr != nil {
		log.Warn("tracing shutdown", "err", terr)
	}
	log.Info("shutdown complete")
	return err
}

// loop runs fn every interval until ctx is done; the first run happens
// after one interval so startup stays fast.
func loop(ctx context.Context, interval time.Duration, fn func(context.Context)) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			fn(ctx)
		}
	}
}
