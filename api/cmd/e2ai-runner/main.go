// Command e2ai-runner is the runner-process entrypoint: config, postgres
// pool, and a River client draining only the "runner" queue with the
// e2ai:run:execute worker mounted (suite.Executor). The runner is
// queue-disjoint from e2ai-api (which drains "default"), mirroring the
// incidents-admin jobs factory split.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/NSXBet/e2ai/api/internal/agents"
	"github.com/NSXBet/e2ai/api/internal/platform/config"
	"github.com/NSXBet/e2ai/api/internal/platform/github"
	"github.com/NSXBet/e2ai/api/internal/platform/postgres"
	"github.com/NSXBet/e2ai/api/internal/platform/river"
	"github.com/NSXBet/e2ai/api/internal/suite"
)

const (
	defaultRunnerImage = "e2ai-runner:local"
	// defaultArtifactsDir matches the tilt/host volume the api serves
	// artifacts from (/e2ai/artifacts/{path}, traversal-guarded, SECR-01).
	defaultArtifactsDir = "/var/lib/e2ai/artifacts"
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
	logger := newLogger(cfg)
	slog.SetDefault(logger)
	logger.Info("e2ai-runner starting", "queue", river.QueueRunner, "max_workers", cfg.RunnerMaxWorkers)

	// The runner has no degraded mode: without postgres it cannot work any
	// job. Bounded retries keep tilt's racing container starts from
	// crash-looping on a slow postgres.
	pool, err := postgres.OpenWithRetry(ctx, logger, cfg.DatabaseURL, 5, 2*time.Second)
	if err != nil {
		return fmt.Errorf("postgres unavailable: %w", err)
	}
	defer pool.Close()

	executor := &suite.Executor{
		Pool:         pool.Pool,
		Image:        getEnv("RUNNER_IMAGE", defaultRunnerImage),
		ArtifactsDir: getEnv("RUNNER_ARTIFACTS_DIR", defaultArtifactsDir),
		Runner:       suite.DockerRunner{},
		Logger:       logger,
	}
	// Check-run reporting is sugar, not evidence: a missing token disables
	// it rather than failing every run (SECR-01 fail-closed for secrets does
	// not extend to optional niceties).
	if cfg.GitHubToken != "" {
		executor.GitHubToken = cfg.GitHubToken
		gh, err := github.New(cfg.GitHubToken)
		if err != nil {
			return fmt.Errorf("create github client: %w", err)
		}
		executor.CheckRuns = checkRunAdapter{gh: gh}
	} else {
		logger.Warn("GITHUB_TOKEN unset; neutral check-runs disabled")
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, executor)

	// e2ai:agent:session worker (Phase 3 T4): spawns one ephemeral
	// agentrunner child per queued agent session. Service token arrives
	// already resolved in the process env (secretspec → 1Password locally,
	// env-to-secret bridge in the cluster — SECR-01: `op read`/`op inject`
	// render secrets, never Go). Optional in dev; missing gateway wiring
	// logs a warning but leaves the worker mounted so sessions still drive
	// (the child itself fails fast and the session records why).
	runnerToken := os.Getenv("E2AI_RUNNER_SERVICE_TOKEN")
	if runnerToken == "" {
		logger.Warn("E2AI_RUNNER_SERVICE_TOKEN unset; agentrunner children get an empty service token")
	}
	apiBase := getEnv("E2AI_API_BASE_URL", "")
	aihubBase := getEnv("AIHUB_BASE_URL", "")
	aihubToken := getEnv("AIHUB_API_TOKEN", "")
	if aihubBase == "" || aihubToken == "" {
		logger.Warn("AIHUB_BASE_URL / AIHUB_API_TOKEN unset; agentrunner children cannot reach the LLM gateway")
	}
	river.AddWorker[agents.AgentSessionArgs](workers, &agents.AgentSessionWorker{
		Pool:   pool.Pool,
		Logger: logger,
		Command: strings.Fields(getEnv(
			"E2AI_AGENTRUNNER_COMMAND", "node /e2ai/agentrunner/agentrunner.mjs")),
		APIBase:       apiBase,
		ServiceToken:  runnerToken,
		AIHUBBase:     aihubBase,
		AIHUBToken:    aihubToken,
		ArtifactsDir:  cfg.ArtifactsRoot,
		RoleModels: map[agents.Role]string{
			agents.RoleNavigator:    getEnv("E2AI_AGENT_MODEL_NAVIGATOR", ""),
			agents.RoleVisionJudge:  getEnv("E2AI_AGENT_MODEL_VISION_JUDGE", ""),
			agents.RolePlanAuthor:   getEnv("E2AI_AGENT_MODEL_PLAN_AUTHOR", ""),
			agents.RoleEdgeHunter:   getEnv("E2AI_AGENT_MODEL_EDGE_HUNTER", ""),
			agents.RoleFlakeAnalyst: getEnv("E2AI_AGENT_MODEL_FLAKE_ANALYST", ""),
		},
		WorkerID: fmt.Sprintf("e2ai-runner:%d", os.Getpid()),
	})

	client, err := river.RunnerClient(pool.Pool, cfg.RunnerMaxWorkers, river.WithWorkers(workers))
	if err != nil {
		return fmt.Errorf("create river runner client: %w", err)
	}
	// The queue reference points at the real client now; the worker only
	// enqueues after Start, so the client is fully built by then.
	executor.Queue = client

	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("start river runner client: %w", err)
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

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// checkRunAdapter bridges github.Client's typed check-run API onto
// suite.CheckRunCreator. Only the check id travels back — the executor
// records it on the run row; status/conclusion later come from the report
// stage (T9), never the creation response.
type checkRunAdapter struct {
	gh *github.Client
}

func (a checkRunAdapter) CreateCheckRun(ctx context.Context, owner, repo string, in suite.CheckRunInput) (*suite.CheckRun, error) {
	cr, err := a.gh.CreateCheckRun(ctx, owner, repo, github.CreateCheckRunInput{
		Name:    in.Name,
		HeadSHA: in.HeadSHA,
		Status:  in.Status,
	})
	if err != nil {
		return nil, err
	}
	return &suite.CheckRun{ID: cr.ID}, nil
}
