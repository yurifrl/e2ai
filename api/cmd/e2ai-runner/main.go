// Command e2ai-runner is the runner-process entrypoint: config, postgres pool,
// and a River client draining only the "runner" queue. Wave 1 registers the
// e2ai:run:execute job arg (checkout SHA -> build -> serve :3000 -> playwright
// run -> parse results.json -> store artifacts).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NSXBet/e2ai/api/internal/platform/config"
	"github.com/NSXBet/e2ai/api/internal/platform/postgres"
	"github.com/NSXBet/e2ai/api/internal/platform/river"
)

func main() {
	if err := run(); err != nil {
		slog.Error("e2ai-runner exited", "error", err)
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	logger.Info("e2ai-runner starting", "queue", river.QueueRunner, "max_workers", cfg.RunnerMaxWorkers)

	pool, err := postgres.OpenWithRetry(ctx, logger, cfg.DatabaseURL, 5, 2*time.Second)
	if err != nil {
		logger.Error("postgres unavailable after retries; exiting", "error", err)
		return err
	}
	defer pool.Close()

	client, err := river.RunnerClient(pool.Pool, cfg.RunnerMaxWorkers)
	if err != nil {
		return err
	}
	if err := client.Start(ctx); err != nil {
		return err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()

	logger.Info("e2ai-runner draining runner queue")
	<-ctx.Done()
	logger.Info("shutdown signal received")
	return nil
}
