// Image-canary tests (PLAT-05) against the scratch Postgres: the inert
// no-op when E2AI_CANARY_IMAGE is unset, the pinned-run creation + one-shot
// runner-queue handoff when it is set, and the explicit-app-filter guard.
package suite

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// canaryEnqueuer captures the execute handoff so the pin travels in the args
// and the queue name in the opts — both provable without a River client.
type canaryEnqueuer struct {
	Args river.JobArgs
	Opts *river.InsertOpts
}

func (e *canaryEnqueuer) Insert(_ context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	e.Args = args
	e.Opts = opts
	return &rivertype.JobInsertResult{}, nil
}

// TestCanaryInertWithoutImage (PLAT-05): no E2AI_CANARY_IMAGE — the job is a
// no-op. No canary run row, no enqueue.
func TestCanaryInertWithoutImage(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	seedAppRun(t, pool)

	q := &canaryEnqueuer{}
	if err := (&CanaryWorker{Pool: pool, Queue: q}).Work(context.Background(), &river.Job[CanaryArgs]{}); err != nil {
		t.Fatalf("Work returned error: %v", err)
	}

	var n int
	if err := pool.QueryRow(context.Background(), `select count(*) from runs where run_number = 0`).Scan(&n); err != nil {
		t.Fatalf("count canary runs: %v", err)
	}
	if n != 0 {
		t.Fatalf("canary run rows after no-op = %d, want 0", n)
	}
	if q.Args != nil {
		t.Fatalf("enqueue happened with args %T, want none", q.Args)
	}
}

// TestCanaryCreatesPinnedRun (PLAT-05): with E2AI_CANARY_IMAGE set the job
// creates exactly one provisioning run (reconcile trigger, run_number 0,
// latest head SHA reused) and enqueues one runner-queue execute pinned to
// the candidate. The run row's runner_image stays empty — the executor
// stamps it when the job actually executes.
func TestCanaryCreatesPinnedRun(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	seedAppRun(t, pool) // the app whose latest head SHA (40 a's) the canary clones

	t.Setenv(envCanaryImage, "e2ai-runner:candidate")

	q := &canaryEnqueuer{}
	if err := (&CanaryWorker{Pool: pool, Queue: q}).Work(context.Background(), &river.Job[CanaryArgs]{}); err != nil {
		t.Fatalf("Work returned error: %v", err)
	}

	var runID uuid.UUID
	var runnerImage, headSHA string
	if err := pool.QueryRow(context.Background(), `
		select id, coalesce(runner_image, ''), head_sha from runs where run_number = 0
	`).Scan(&runID, &runnerImage, &headSHA); err != nil {
		t.Fatalf("load canary run: %v", err)
	}
	if runnerImage != "" {
		t.Fatalf("canary run runner_image = %q, want empty (executor stamps it at execute time)", runnerImage)
	}
	if headSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("canary run head_sha = %q, want the app's latest webhook-run SHA", headSHA)
	}

	execArgs, ok := q.Args.(ExecuteArgs)
	if !ok {
		t.Fatalf("enqueue args = %T, want ExecuteArgs", q.Args)
	}
	if execArgs.RunID != runID.String() {
		t.Fatalf("enqueue RunID = %q, want the canary run %s", execArgs.RunID, runID)
	}
	if execArgs.Image != "e2ai-runner:candidate" {
		t.Fatalf("enqueue Image = %q, want the candidate pin", execArgs.Image)
	}
	if q.Opts == nil || q.Opts.Queue != "runner" || q.Opts.MaxAttempts != 1 {
		t.Fatalf("enqueue opts = %+v, want runner queue, one shot", q.Opts)
	}
}

// TestCanaryExplicitAppUnknown (PLAT-05): an explicit E2AI_CANARY_APP that
// matches no registered app is a configuration error, not a soft skip — a
// rollout that thinks it is running must fail loudly.
func TestCanaryExplicitAppUnknown(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	seedAppRun(t, pool)

	t.Setenv(envCanaryImage, "e2ai-runner:candidate")
	t.Setenv(envCanaryApp, "NSXBet/no-such-app")

	q := &canaryEnqueuer{}
	if err := (&CanaryWorker{Pool: pool, Queue: q}).Work(context.Background(), &river.Job[CanaryArgs]{}); err == nil {
		t.Fatal("Work returned nil error for unknown app filter, want failure")
	}
}
