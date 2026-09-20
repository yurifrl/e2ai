// Command e2ai-api is the e2ai service entrypoint: config, postgres pool,
// River default-queue client, and the HTTP surface (healthz first; webhook and
// Connect handlers land in Wave 1).
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

	"github.com/NSXBet/e2ai/api/internal/platform/config"
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

	// River default-queue client (webhook/report stages drain here). The schema
	// migration runs with Wave 1; until then river.Start starts fine against an
	// empty database.
	var riverClient *river.Client
	if pool != nil {
		riverClient, err = river.DefaultClient(pool.Pool, 2)
		if err != nil {
			return err
		}
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
		newMux(dbHealth),
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

// newMux returns the api HTTP surface. Wave 1 mounts /github/webhook and the
// Connect handlers under /e2ai/; the scaffold ships only liveness/health.
func newMux(dbHealth func() bool) http.Handler {
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
