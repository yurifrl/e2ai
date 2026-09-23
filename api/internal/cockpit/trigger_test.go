// Manual-trigger tests (06-02 T1): request-shape validation is pure unit
// coverage; the pause / zero-ceiling / insert-as-gate behaviors run against
// the scratch Postgres (testPool + budgetApp conventions from state_test.go /
// budget_test.go) with a real River client so the enqueue-inside-tx shape is
// exercised end to end.
package cockpit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/ingest"
	platriver "github.com/NSXBet/e2ai/api/internal/platform/river"
	"github.com/NSXBet/e2ai/api/internal/suite"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// --- request-shape validation (pure, no DB) ---

// testAppUUID is any syntactically valid app id for the pure validation cases.
const testAppUUID = "6a3d5937-df83-4a02-a8b8-6f77ba1ee8e5"

func TestTriggerRequestValidation(t *testing.T) {
	cases := []struct {
		name    string
		req     TriggerRequest
		wantErr bool
	}{
		{"missing app", TriggerRequest{HeadSHA: "abc", Environment: "staging", PlanVersionID: "e6a1e35e-8b40-4a3a-8f1a-2f7f5f7f1e01"}, true},
		{"missing head", TriggerRequest{AppID: testAppUUID, Environment: "staging", PlanVersionID: "e6a1e35e-8b40-4a3a-8f1a-2f7f5f7f1e01"}, true},
		{"plain ok", TriggerRequest{AppID: testAppUUID, HeadSHA: "abc", Environment: "staging", PlanVersionID: "e6a1e35e-8b40-4a3a-8f1a-2f7f5f7f1e01"}, false},
		{"retest needs no sha", TriggerRequest{AppID: testAppUUID, Environment: "staging", Mode: ModeRetest, PlanVersionID: "e6a1e35e-8b40-4a3a-8f1a-2f7f5f7f1e01"}, false},
		{"reproduce missing source", TriggerRequest{AppID: testAppUUID, HeadSHA: "abc", Environment: "staging", Mode: ModeReproduce, PlanVersionID: "e6a1e35e-8b40-4a3a-8f1a-2f7f5f7f1e01"}, true},
		{"unknown mode", TriggerRequest{AppID: testAppUUID, HeadSHA: "abc", Environment: "staging", Mode: "yolo", PlanVersionID: "e6a1e35e-8b40-4a3a-8f1a-2f7f5f7f1e01"}, true},
	}
	for _, tc := range cases {
		err := tc.req.validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
}

// --- DB-backed behavior (testPool convention; skip without the harness) ---

func triggerService(t *testing.T) *Service {
	t.Helper()
	pool := testPool(t)
	// The trigger path enqueues ExecuteArgs onto river_job inside its
	// transaction, so the River schema must exist (MigrateRiver is
	// idempotent) — same setup convention as ingest's gateWorker.
	if err := app.MigrateRiver(context.Background(), pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	workers := river.NewWorkers()
	river.AddWorker[suite.ExecuteArgs](workers, noopExecuteWorker{})
	client, err := platriver.RunnerClient(pool, 1, platriver.WithWorkers(workers))
	if err != nil {
		t.Fatalf("river test client: %v", err)
	}
	return NewService(pool, "", nil, client, nil, ingest.NewStore(pool), nil)
}

func TestTriggerZeroCeilingRefuses(t *testing.T) {
	s := triggerService(t)
	zero := int64(0)
	appID := budgetApp(t, s.Pool, &zero)

	_, _, err := s.Trigger(context.Background(), TriggerRequest{
		AppID:         uuid.UUID(appID.Bytes).String(),
		HeadSHA:       "abc",
		Environment:   "test",
		PlanVersionID: seededPlanVersion(t, s.Pool, appID),
	})
	if !errors.Is(err, ErrZeroCeiling) {
		t.Fatalf("zero ceiling must refuse with ErrZeroCeiling, got %v", err)
	}
}

func TestTriggerPausedRefuses(t *testing.T) {
	s := triggerService(t)
	appID := budgetApp(t, s.Pool, nil)

	if err := SetPaused(context.Background(), s.Pool, "op@example.com", true, "release freeze"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	_, _, err := s.Trigger(context.Background(), TriggerRequest{
		AppID:         uuid.UUID(appID.Bytes).String(),
		HeadSHA:       "abc",
		Environment:   "staging",
		PlanVersionID: seededPlanVersion(t, s.Pool, appID),
	})
	if !errors.Is(err, ErrPaused) || !strings.Contains(err.Error(), "release freeze") {
		t.Fatalf("paused trigger must carry the pause reason, got %v", err)
	}
}

func TestTriggerRacingInsertAsGate(t *testing.T) {
	s := triggerService(t)
	appID := budgetApp(t, s.Pool, nil)
	ctx := context.Background()

	// Two racing triggers on one app: the 0010 partial unique index is the
	// gate — exactly one wins (a provisioning manual run), the loser
	// surfaces ErrActiveManualRun (P3: one 202, one 409).
	pin := seededPlanVersion(t, s.Pool, appID)
	var mu sync.Mutex
	var wins, refusals int
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := TriggerRequest{
				AppID:         uuid.UUID(appID.Bytes).String(),
				HeadSHA:       "abc",
				Environment:   "staging",
				PlanVersionID: pin,
			}
			_, _, err := s.Trigger(ctx, req)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrActiveManualRun):
				refusals++
			default:
				t.Errorf("unexpected trigger error: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || refusals != 1 {
		t.Fatalf("racing triggers must yield one 202 one 409, got wins=%d refusals=%d", wins, refusals)
	}
}

func TestTriggerInsertsManualRunAtProvisioning(t *testing.T) {
	s := triggerService(t)
	appID := budgetApp(t, s.Pool, nil)

	runID, wfrID, err := s.Trigger(context.Background(), TriggerRequest{
		AppID:         uuid.UUID(appID.Bytes).String(),
		HeadSHA:       "abc",
		Environment:   "staging",
		PlanVersionID: seededPlanVersion(t, s.Pool, appID),
		actor:         "operator@example.com",
	})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if wfrID >= 0 {
		t.Fatalf("manual workflow_run_id must be a negative synthetic id, got %d", wfrID)
	}

	ctx := context.Background()
	var source, triggeredBy, status string
	if err := s.Pool.QueryRow(ctx,
		`select trigger_source, triggered_by, status from runs where id = $1`, pgUUID(runID)).
		Scan(&source, &triggeredBy, &status); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	if source != "manual" || triggeredBy != "operator@example.com" || status != "provisioning" {
		t.Fatalf("manual run row = (%s, %s, %s), want (manual, operator@example.com, provisioning)",
			source, triggeredBy, status)
	}

	// The pinned plan resolution was stamped at trigger time.
	var mode, resolutionJSON string
	if err := s.Pool.QueryRow(ctx,
		`select mode, resolution::text from run_plan_resolutions where run_id = $1`, pgUUID(runID)).
		Scan(&mode, &resolutionJSON); err != nil {
		t.Fatalf("read pinned resolution: %v", err)
	}
	if mode != "approved" || !strings.Contains(resolutionJSON, "manual_pinned") {
		t.Fatalf("pinned resolution = (mode %s, %s), want approved/manual_pinned", mode, resolutionJSON)
	}
}

// --- test scaffolding ---

// noopExecuteWorker satisfies River's worker registration requirement for the
// runner queue; the trigger path only inserts the job, never runs it.
type noopExecuteWorker struct {
	river.WorkerDefaults[suite.ExecuteArgs]
}

func (noopExecuteWorker) Work(context.Context, *river.Job[suite.ExecuteArgs]) error { return nil }

// --- seed helpers ---

// pgUUID converts a uuid.UUID to its pgtype form for query parameters.
func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// seedPlanContent is the minimal parseable plan body the seeded version
// stores; same shape as plans/parse_test.go's fixtures.
const seedPlanContent = `---
id: seed-v1.0.0
app: seed-app
title: seeded plan
surfaces: [web]
trigger: on-demand
---
1. Open the live page.
   judge: the badge is visible
`

// seededPlanVersion inserts a plans row joined to appID plus one approved
// plan_versions row, returning the version id the trigger pins against.
// resolvePin joins plans on app_id, so the plan must belong to the same app
// the test triggers.
func seededPlanVersion(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID) string {
	t.Helper()
	vid := uuid.New()
	planID := "seed-plan-" + vid.String()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into plans (id, app_id, source_path, status, latest_version_id)
		values ($1, $2, 'plans/seed.md', 'approved', $3)`,
		planID, appID, vid); err != nil {
		t.Fatalf("seed plans row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into plan_versions (id, plan_id, version, content_hash, content, frontmatter, status, sources, created_by)
		values ($1, $2, '1.0.0', $3, $4, $5, 'approved', '[]', 'trigger-test')`,
		vid, planID, "hash-"+vid.String(), seedPlanContent,
		`{"id":"seed-v1.0.0","app":"seed-app","title":"seeded plan","surfaces":["web"],"trigger":"on-demand"}`); err != nil {
		t.Fatalf("seed plan_versions row: %v", err)
	}
	return vid.String()
}
