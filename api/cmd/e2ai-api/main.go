// Command e2ai-api is the e2ai service entrypoint: config, postgres pool,
// River default-queue client (with the ingest webhook + reconcile sweep), and
// the HTTP surface (healthz, GitHub webhook, Connect handlers under /e2ai/).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/ingest"
	"github.com/NSXBet/e2ai/api/internal/platform/config"
	"github.com/NSXBet/e2ai/api/internal/platform/github"
	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
	"github.com/NSXBet/e2ai/api/internal/platform/postgres"
	"github.com/NSXBet/e2ai/api/internal/platform/river"
)

func main() {
	if err := run(); err != nil {
		slog.Error("e2ai-api exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config.LoadDotEnv()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg)
	slog.SetDefault(logger)
	logger.Info("e2ai-api starting", "port", cfg.Port, "log_level", cfg.LogLevel)

	// Bounded pool connect: a missing local postgres is a degraded start
	// (healthz flips to 503), not a crash — tilt pods come up on their own
	// schedule.
	pool, err := postgres.OpenWithRetry(ctx, logger, cfg.DatabaseURL, 5, 2*time.Second)
	if err != nil {
		logger.Error("postgres unavailable after retries; running degraded", "error", err)
	}
	defer func() {
		if pool != nil {
			pool.Close()
		}
	}()

	// Ingest + River default-queue client (webhook ingest and the reconcile
	// sweep drain here). Schema migrations and registry seeding run first
	// (T3): the client needs the river_* tables, workers need registered_apps.
	var webhook http.Handler
	var riverClient *river.Client
	if pool != nil {
		if err := app.Bootstrap(ctx, pool.Pool, logger, cfg.RegistryDir); err != nil {
			return err
		}
		apps, err := app.LoadRegistry(cfg.RegistryDir)
		if err != nil {
			return err
		}

		// GitHub REST client drives the reconciliation sweep (webhook
		// delivery is at-least-once, never guaranteed). A missing token is a
		// degraded sweep, not a crash: webhooks alone keep ingesting.
		ghClient, ghErr := github.New(cfg.GitHubToken)
		if ghErr != nil {
			logger.Warn("github client unavailable; reconciliation sweep disabled", "error", ghErr)
		}

		// The worker bundle is built before the client (the client config
		// takes it), while workers enqueue transactionally through a QueueRef
		// bound to the client right after construction.
		store := ingest.NewStore(pool.Pool)
		queueRef := &ingest.QueueRef{}
		workers := river.NewWorkers()
		var periodic []*river.PeriodicJob
		if ghClient != nil {
			river.AddWorker(workers, ingest.NewReconcileWorker(apps, ghClient, store, queueRef, logger))
			periodic = append(periodic, river.NewPeriodicJob(
				ingest.ReconcileInterval(),
				func() river.JobArgs { return ingest.ReconcileArgs{} },
				true,
			))
		}
		opts := []river.Option{river.WithWorkers(workers)}
		if len(periodic) > 0 {
			opts = append(opts, river.WithPeriodicJobs(periodic...))
		}
		riverClient, err = river.DefaultClient(pool.Pool, 2, opts...)
		if err != nil {
			return err
		}
		queueRef.Set(riverClient)
		webhook = ingest.NewHandler(cfg.WebhookSecret, apps, store, riverClient, logger)

		if err := riverClient.Start(ctx); err != nil {
			return err
		}
		defer func() {
			// Graceful drain with a bounded wait; the pool defer below runs
			// after this (LIFO) so the client never outlives its pool.
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = riverClient.Stop(stopCtx)
		}()
	}

	var dbHealth func() bool
	if pool != nil {
		dbHealth = func() bool { return pool.Healthy(ctx) }
	} else {
		dbHealth = func() bool { return false }
	}
	handler := httpx.IdentityWithConfig(
		httpx.IdentityConfig{DevFallbackUser: cfg.DevFallbackUser},
		newMux(dbHealth, webhook),
	)
	server := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	logger.Info("e2ai-api listening", "addr", server.Addr, "postgres", pool != nil)

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// newMux returns the api HTTP surface: liveness/health plus the GitHub
// webhook. A nil webhook (postgres down) leaves the route unmounted rather
// than accepting unverifiable deliveries.
func newMux(dbHealth func() bool, webhook http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if dbHealth() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"degraded","postgres":false}`))
	})
	if webhook != nil {
		mux.Handle("/github/webhook", webhook)
		mux.Handle("/github/webhook/", webhook)
	}
	return mux
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
