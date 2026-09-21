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

	"github.com/NSXBet/e2ai/api/internal/agents"
	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/approvals"
	"github.com/NSXBet/e2ai/api/internal/derive"
	"github.com/NSXBet/e2ai/api/internal/flakes"
	"github.com/NSXBet/e2ai/api/internal/ingest"
	"github.com/NSXBet/e2ai/api/internal/mcp"
	"github.com/NSXBet/e2ai/api/internal/plans"
	"github.com/NSXBet/e2ai/api/internal/platform/config"
	"github.com/NSXBet/e2ai/api/internal/platform/github"
	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
	"github.com/NSXBet/e2ai/api/internal/platform/postgres"
	"github.com/NSXBet/e2ai/api/internal/platform/river"
	"github.com/NSXBet/e2ai/api/internal/report"
	"github.com/NSXBet/e2ai/api/internal/suite"
	"github.com/NSXBet/e2ai/api/internal/trends"
	"github.com/jackc/pgx/v5"
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
	var ghClient *github.Client
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
		gh, ghErr := github.New(cfg.GitHubToken)
		if ghErr != nil {
			logger.Warn("github client unavailable; reconciliation sweep disabled", "error", ghErr)
		}
		ghClient = gh

		// The worker bundle is built before the client (the client config
		// takes it), while workers enqueue transactionally through a QueueRef
		// bound to the client right after construction.
		store := ingest.NewStore(pool.Pool)
		queueRef := &ingest.QueueRef{}
		workers := river.NewWorkers()

		// Plans/corpus (T4): the sync worker mirrors declarative plans into
		// the catalog under content-hash identity; corpus seeding walks the
		// spec oracle after each sync pass. A missing GitHub token leaves
		// both mounted but inert — sync needs the contents API.
		plans.AddWorkers(workers, pool.Pool, ghClient, logger)

		// Derive tail (DERV-01/02): after each ingest-created run, resolve
		// the changed paths and store derived draft plan versions through
		// the plans draft-upsert seam. Never executes anything — drafts
		// land in the approval queue (APPR-01). Inert without a GitHub
		// client (the worker reports the degraded PR resolution).
		river.AddWorker[derive.DeriveDraftArgs](workers, derive.NewDeriver(pool.Pool, ghClient, logger))

		// Rollout gate (INGE-03): the first pipeline job after ingest —
		// polls the registered app's health_url until the deploy is live,
		// then hands the run to the runner queue. Needs no GitHub client,
		// so it is registered unconditionally.
		river.AddWorker(workers, ingest.NewWaitRolloutWorker(pool.Pool, queueRef, logger))

		// Report tail (VERD-01/SUIT-02): derives the run verdict from the
		// evidence the runner stored, persists it with the summary counts,
		// and completes the run. When a GitHub token is present the T10
		// publisher rides the job's Publisher seam: sticky PR comment +
		// neutral check-run + signed report link. Without a token the job
		// still runs (verdict persistence is independent of publishing).
		reportWorker := &report.Worker{Pool: pool.Pool, Logger: logger}
		if ghClient != nil {
			reportWorker.Publisher = report.NewPublisher(
				ghClient,
				cfg.ReportBaseURL,
				cfg.ReportLinkSecret,
				logger,
			)
		}
		river.AddWorker(workers, reportWorker)

		// Agent-session orphan sweep (AGNT-02): the periodic half of T2's
		// wiring — lapsed claimed/running sessions are marked failed and a
		// fresh queued session is requeued atomically.
		river.AddWorker[agents.SweepArgs](workers, &agents.SweepWorker{Pool: pool.Pool, Logger: logger})

		// Edge sweeps (EDGE-01, T4): per-app opt-in cadence from the
		// registry — the 60s tick only bounds how long a due app waits
		// past its own interval.
		river.AddWorker[agents.EdgesweepArgs](workers, &agents.EdgesweepWorker{Pool: pool.Pool, Logger: logger})

		// Flake Analyst trigger (FLAK-01/02, T4): flake-rate threshold
		// crossings fire alerts + analyst sessions, cooldown-deduped.
		river.AddWorker[agents.FlakesTickArgs](workers, &agents.FlakesTickWorker{Pool: pool.Pool, Logger: logger})

		// Image canary (PLAT-05): slow-cadence periodic job that runs a
		// candidate runner image through the real pinning path (creates a
		// run, enqueues execute with the pinned image). Inert unless
		// E2AI_CANARY_IMAGE is set — read at fire time, so this wiring is
		// harmless in every environment that hasn't opted into a rollout.
		canaryWorker := &suite.CanaryWorker{Pool: pool.Pool, Logger: logger}
		river.AddWorker[suite.CanaryArgs](workers, canaryWorker)

		var periodic []*river.PeriodicJob
		periodic = append(periodic, river.NewPeriodicJob(
			agents.EdgesweepInterval(),
			func() river.JobArgs { return agents.SweepArgs{} },
			true,
		))
		periodic = append(periodic, river.NewPeriodicJob(
			agents.EdgesweepInterval(),
			func() river.JobArgs { return agents.EdgesweepArgs{} },
			true,
		))
		periodic = append(periodic, river.NewPeriodicJob(
			agents.FlakesTickInterval(),
			func() river.JobArgs { return agents.FlakesTickArgs{} },
			true,
		))
		periodic = append(periodic, river.NewPeriodicJob(
			suite.CanaryInterval(),
			func() river.JobArgs { return suite.CanaryArgs{} },
			true,
		))
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
		canaryWorker.Queue = riverClient // late-bound like QueueRef; set before Start
		webhook = ingest.NewHandler(cfg.WebhookSecret, apps, store, riverClient, logger).
			// APPS-03: per-app webhook secrets — resolve every registry
			// app's op:// ref at boot (SECR-01: the vault CLI renders
			// secrets, never Go); apps whose ref fails to render fall
			// back to the shared secret.
			WithSecretResolver(ctx, ingest.OpSecretResolver(), apps).
			// EDGE-01: signature-valid deliveries from unknown apps are
			// recorded for admin triage instead of silently dropped.
			WithTriage(ingest.NewTriage(pool.Pool))

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

	// T11 runs HTTP surface: paginated run list, run detail, report-page
	// data, and token/identity-gated artifact serving. Unmounted when
	// postgres is down (no run data to serve).
	var runsSurface *report.RunsHTTP
	if pool != nil {
		runsSurface = report.NewRunsHTTP(pool.Pool, cfg.ArtifactsRoot, cfg.ReportLinkSecret)
	}

	// T4 plans/corpus HTTP surface: plan catalog, version content/diff,
	// manual sync trigger, and the corpus oracle. Same degraded posture:
	// unmounted when postgres is down. The sync trigger needs the GitHub
	// contents API; without a token the route answers 503 from the handler.
	var plansSurface *plans.PlansHTTP
	if pool != nil {
		plansSurface = plans.NewPlansHTTP(pool.Pool, ghClient)
	}

	// APPR-01/02 approvals HTTP surface: pending queue + the two attributed
	// decision endpoints. Same degraded posture — unmounted when postgres
	// is down.
	var approvalsSurface *approvals.ApprovalsHTTP
	if pool != nil {
		approvalsSurface = approvals.NewApprovalsHTTP(pool.Pool, cfg.DevFallbackUser)
	}

	// Runner service token (AGNT-03): the second credential half for the
	// agent-session lifecycle endpoints. Optional in dev — a missing token
	// leaves the endpoints on BFF identity alone.
	runnerToken := os.Getenv("E2AI_RUNNER_SERVICE_TOKEN")
	if runnerToken == "" && pool != nil {
		logger.Warn("E2AI_RUNNER_SERVICE_TOKEN unset; runner endpoints accept BFF identity only")
	}

	// AGNT-03 agents HTTP surface: runner lifecycle endpoints (heartbeat,
	// steps, complete, fail) gated by the runner service token in addition
	// to BFF identity, plus the admin kill switch and session enqueue. Same
	// degraded posture — unmounted when postgres is down.
	var agentsSurface *agents.AgentsHTTP
	if pool != nil {
		agentsSurface = agents.NewAgentsHTTP(pool.Pool, runnerToken)
	}

	// FLAK-01/02 flake-governance surface: flake-rate analytics + the
	// quarantine store. Same degraded posture — unmounted when postgres
	// is down.
	var flakesSurface *flakes.QuarantineHTTP
	if pool != nil {
		flakesSurface = flakes.NewQuarantineHTTP(pool.Pool)
	}

	// TREN-01 trends surface: verdict-flip rate, per-run agent cost, and
	// pass/fail history aggregations. Same degraded posture.
	var trendsSurface *trends.TrendsHTTP
	if pool != nil {
		trendsSurface = trends.NewTrendsHTTP(pool.Pool)
	}

	// MCP surface (MCP-01): read-only JSON-RPC for coding agents in PRs.
	// Same degraded posture.
	var mcpSurface *mcp.Surface
	if pool != nil {
		mcpSurface = mcp.New(pool.Pool, runnerToken, cfg.ReportBaseURL, cfg.ReportLinkSecret)
	}

	handler := httpx.IdentityWithConfig(
		httpx.IdentityConfig{DevFallbackUser: cfg.DevFallbackUser},
		httpx.RequestLog(httpx.RedactSecrets(logger, map[string]string{
			"WEBHOOK_SECRET":            cfg.WebhookSecret,
			"GITHUB_TOKEN":              cfg.GitHubToken,
			"E2AI_REPORT_LINK_SECRET":   cfg.ReportLinkSecret,
			"E2AI_RUNNER_SERVICE_TOKEN": runnerToken,
		}), newMux(dbHealth, webhook, runsSurface, plansSurface, approvalsSurface, agentsSurface, flakesSurface, trendsSurface, mcpSurface, pool, riverClient)),
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

// newMux returns the api HTTP surface: liveness/health, the GitHub webhook,
// and (when postgres is up) the T11 runs + artifacts surface.
func newMux(dbHealth func() bool, webhook http.Handler, runs *report.RunsHTTP, plans *plans.PlansHTTP, approvals *approvals.ApprovalsHTTP, agentSessions *agents.AgentsHTTP, flakesSurface *flakes.QuarantineHTTP, trendsSurface *trends.TrendsHTTP, mcpSurface *mcp.Surface, pool *postgres.Pool, riverClient *river.Client) http.Handler {
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
	if runs != nil {
		// Wildcard patterns make r.PathValue resolve inside the handlers;
		// method prefixes keep the surface GET-only.
		mux.HandleFunc("GET /api/runs", runs.ListRuns)
		mux.HandleFunc("GET /api/runs/{id}", runs.GetRun)
		mux.HandleFunc("GET /api/runs/{id}/report", runs.RunReport)
		mux.HandleFunc("GET /artifacts/{runID}/{path...}", runs.ServeArtifact)
	}
	if plans != nil {
		// Plans/corpus surface (T4): catalog + versions + diff (GET, view
		// permission), sync trigger + corpus decisions (POST, edit
		// permission).
		plans.Mount(mux)
	}
	if approvals != nil {
		// Approvals surface (APPR-01/02): queue + attributed decisions.
		approvals.Mount(mux)
	}
	if flakesSurface != nil {
		// Flake governance surface (FLAK-01/02): rates + quarantine.
		flakesSurface.Mount(mux)
	}
	if trendsSurface != nil {
		// Trends analytics surface (TREN-01).
		trendsSurface.Mount(mux)
	}
	if mcpSurface != nil {
		// MCP read-only surface (MCP-01): JSON-RPC for coding agents.
		mcpSurface.Mount(mux)
	}
	if agentSessions != nil {
		// Agents surface (AGNT-03): runner lifecycle (service token), the
		// admin kill switch + session enqueue, and the queue data listing.
		agentSessions.Mount(mux)
	}
	if pool != nil && pool.Pool != nil {
		// Plan resolution + derive endpoints (T8, APPR-01/DERV-01): the
		// resolution record is the run-scoped evidence trail the executor's
		// gate wrote; derive-draft is a manual re-derivation trigger.
		mux.HandleFunc("GET /api/runs/{id}/plan-resolution", func(w http.ResponseWriter, r *http.Request) {
			identity, ok := httpx.IdentityFromContext(r.Context())
			switch {
			case !ok:
				httpErr(w, http.StatusUnauthorized, "unauthorized")
				return
			case !identity.HasPermission(httpx.RunViewPermission):
				httpErr(w, http.StatusForbidden, "forbidden")
				return
			}
			var resolution []byte
			err := pool.Pool.QueryRow(r.Context(),
				`select resolution from run_plan_resolutions where run_id = $1`,
				r.PathValue("id")).Scan(&resolution)
			if errors.Is(err, pgx.ErrNoRows) {
				httpErr(w, http.StatusNotFound, "no plan resolution recorded for this run")
				return
			}
			if err != nil {
				httpErr(w, http.StatusInternalServerError, "read resolution failed")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(resolution)
		})
		mux.HandleFunc("POST /api/runs/{id}/derive-draft", func(w http.ResponseWriter, r *http.Request) {
			identity, ok := httpx.IdentityFromContext(r.Context())
			switch {
			case !ok:
				httpErr(w, http.StatusUnauthorized, "unauthorized")
				return
			case !identity.HasPermission(httpx.RunEditPermission):
				httpErr(w, http.StatusForbidden, "forbidden")
				return
			}
			if riverClient == nil {
				httpErr(w, http.StatusServiceUnavailable, "queue unavailable")
				return
			}
			if _, err := riverClient.Insert(r.Context(),
				derive.DeriveDraftArgs{RunID: r.PathValue("id"), PRNumber: 0},
				derive.DeriveOpts()); err != nil {
				httpErr(w, http.StatusInternalServerError, "enqueue derive failed")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"enqueued":true}`))
		})
	}
	return mux
}

// httpErr writes the surface's JSON error shape.
func httpErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":` + strconv.Quote(msg) + `}`))
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
