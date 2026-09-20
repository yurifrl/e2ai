// Reconciliation sweep (INGE-02): GitHub webhooks are at-least-once, never
// guaranteed. A periodic River job lists recent completed workflow_dispatch
// runs for every registered workflow straight from the Actions REST API and
// feeds anything missing through the webhook's dedupe path, so a missed
// delivery self-heals within one tick instead of never.
package ingest

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/platform/github"
	"github.com/riverqueue/river"
)

const (
	// KindReconcile is the periodic sweep job kind (default queue).
	KindReconcile = "e2ai:ingest:reconcile"

	// reconcileInterval is the sweep cadence: five minutes covers GitHub's
	// documented webhook gaps with tolerable ingest latency.
	reconcileInterval = 5 * time.Minute

	// reconcileLookback covers GitHub webhook outages and deploys that
	// happened while e2ai was down; dedupe makes re-walking this window free.
	reconcileLookback = 24 * time.Hour
)

// ReconcileInterval exposes the sweep cadence for main.go's periodic wiring.
func ReconcileInterval() time.Duration { return reconcileInterval }

// ReconcileArgs is the periodic sweep job's payload (stateless).
type ReconcileArgs struct{}

// Kind implements river.JobArgs.
func (ReconcileArgs) Kind() string { return KindReconcile }

// RemoteRun aliases platform/github's run shape: identical struct, so
// *github.Client satisfies RunsLister structurally and main.go wires it
// directly.
type RemoteRun = github.RemoteRun

// RunsLister lists recent completed workflow_dispatch runs of one registered
// workflow from the GitHub REST API. platform/github's Client implements it;
// tests use a fake.
type RunsLister interface {
	ListCompletedRuns(ctx context.Context, owner, repo, workflowName string, since time.Time) ([]RemoteRun, error)
}

// ReconcileWorker is the sweep worker: for every registered workflow, list
// recent completed dispatch runs from GitHub and ingest the missing ones
// through the webhook's dedupe path. Idempotent: replayed runs land on the
// same run row via the (app_id, workflow_run_id) unique constraint.
type ReconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]

	matchers []matcher
	lister   RunsLister
	store    *Store
	queue    JobEnqueuer
	logger   *slog.Logger
}

// NewReconcileWorker wires the sweep. queue is typically a *QueueRef bound to
// the River client right after client construction.
func NewReconcileWorker(apps []app.App, lister RunsLister, store *Store, queue JobEnqueuer, logger *slog.Logger) *ReconcileWorker {
	return &ReconcileWorker{
		matchers: compileMatchers(apps, logger),
		lister:   lister,
		store:    store,
		queue:    queue,
		logger:   logger,
	}
}

// Work runs one sweep pass. A single app's GitHub failure is logged and
// remembered (one pass = one lister error → River retry with backoff) but
// must not starve the other registered workflows; ingest errors per run are
// logged and carried the same way.
func (w *ReconcileWorker) Work(ctx context.Context, job *river.Job[ReconcileArgs]) error {
	var firstErr error
	for _, m := range w.matchers {
		runs, err := w.lister.ListCompletedRuns(ctx, m.app.Owner, m.app.Repo, m.app.WorkflowName,
			time.Now().Add(-reconcileLookback))
		if err != nil {
			w.logger.Error("reconcile: list runs failed",
				"app", m.app.WorkflowName, "environment", m.app.Environment, "error", err)
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		for _, run := range runs {
			if run.Event != "workflow_dispatch" || run.Conclusion != "success" || !m.re.MatchString(run.DisplayTitle) {
				continue
			}
			result, err := w.store.Ingest(ctx, sweepSeed(m.app, run), w.queue)
			switch {
			case err != nil:
				w.logger.Error("reconcile: ingest failed", "workflow_run_id", run.ID, "error", err)
				firstErr = cmp.Or(firstErr, err)
			case result == ResultAccepted:
				w.logger.Info("reconcile: ingested missed run",
					"workflow_run_id", run.ID, "app", m.app.WorkflowName)
			}
		}
	}
	return firstErr
}

// sweepSeed builds the ingest seed for a remotely discovered run. The
// delivery id is namespaced under reconcile: so sweep recordings never
// collide with real webhook deliveries; the payload mirrors the webhook's
// workflow_run shape so webhook_events stays a coherent evidence trail.
func sweepSeed(a app.App, run RemoteRun) RunSeed {
	payload, err := json.Marshal(map[string]any{
		"swept":    true,
		"workflow": map[string]any{"name": a.WorkflowName},
		"workflow_run": map[string]any{
			"id":            run.ID,
			"run_number":    run.RunNumber,
			"head_sha":      run.HeadSHA,
			"head_branch":   run.HeadBranch,
			"display_title": run.DisplayTitle,
			"html_url":      run.HTMLURL,
			"event":         run.Event,
			"conclusion":    run.Conclusion,
		},
	})
	if err != nil {
		// Marshaling a literal map of plain values cannot fail; keep the
		// worker path panic-free by falling back to a minimal document.
		payload = []byte(fmt.Sprintf(`{"swept":true,"workflow_run":{"id":%d}}`, run.ID))
	}
	return RunSeed{
		App:            a,
		DeliveryID:     fmt.Sprintf("reconcile:%d", run.ID),
		WorkflowRunID:  run.ID,
		RunNumber:      run.RunNumber,
		HeadSHA:        run.HeadSHA,
		HeadBranch:     run.HeadBranch,
		TriggerSource:  TriggerReconcile,
		SignatureValid: false,
		Payload:        payload,
		Enqueue:        true,
	}
}
