// Plan resolution gate tests (APPR-01, T8) against the scratch Postgres:
// approved-only execution, the changed-plan rule (last approved keeps
// running), the legacy path (no plans at all), and the resolution record
// round-trip the REST surface reads.
package suite

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// planVersionID returns the uuid of a plan's named version.
func planVersionID(t *testing.T, pool *pgxpool.Pool, planID, version string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`select id from plan_versions where plan_id = $1 and version = $2`, planID, version).
		Scan(&id); err != nil {
		t.Fatalf("version %s@%s: %v", planID, version, err)
	}
	return id
}

// addVersion inserts a version row over an existing plan and returns its id.
func addVersion(t *testing.T, pool *pgxpool.Pool, appID uuid.UUID, planID, version, status string) uuid.UUID {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into plan_versions (plan_id, version, content_hash, content, frontmatter, status, created_by)
		values ($1, $2, $3, $4, $5, $6, $7)
	`, planID, version, "hash-"+version, "content "+version,
		`{"id":"`+planID+`"}`, status, "reviewer"); err != nil {
		t.Fatalf("insert version %s@%s: %v", planID, version, err)
	}
	return planVersionID(t, pool, planID, version)
}

func seedPlanRow(t *testing.T, pool *pgxpool.Pool, appID uuid.UUID, planID, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into plans (id, app_id, source_path, status) values ($1, $2, $3, $4)
	`, planID, appID, "e2ai/plans/"+planID+".md", status); err != nil {
		t.Fatalf("insert plan %s: %v", planID, err)
	}
}

func approveVersion(t *testing.T, pool *pgxpool.Pool, vid uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into plan_approvals (plan_version_id, decision, approver) values ($1, 'approved', 'reviewer')
	`, vid); err != nil {
		t.Fatalf("approve version: %v", err)
	}
}

// runApp returns the run's registered app id.
func runApp(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) uuid.UUID {
	t.Helper()
	var appID uuid.UUID
	if err := pool.QueryRow(context.Background(), `select app_id from runs where id = $1`, runID).Scan(&appID); err != nil {
		t.Fatalf("run app: %v", err)
	}
	return appID
}

// loadResolution reads the run's stored resolution record.
func loadResolution(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) (primary *string, mode string, selected, resolution []byte) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		select plan_id, mode, selected, resolution from run_plan_resolutions where run_id = $1
	`, runID).Scan(&primary, &mode, &selected, &resolution); err != nil {
		t.Fatalf("load run_plan_resolutions: %v", err)
	}
	return
}

// resolutionRecord mirrors the stored resolution jsonb.
type resolutionRecord struct {
	Selected []selectedPlan `json:"selected"`
	Excluded []excludedPlan `json:"excluded"`
	Trunc    *bool          `json:"diff_truncated"`
}

// envRunner records the container spec so the gate's env wiring is provable.
type envRunner struct{ Specs []ContainerSpec }

func (r *envRunner) Run(_ context.Context, spec ContainerSpec) error {
	r.Specs = append(r.Specs, spec)
	return nil
}

// gateExecutor builds an Executor like storeExecutor but with a capturing
// runner and the fixture results pre-placed in the per-run artifacts dir.
func gateExecutor(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) (*Executor, *envRunner) {
	t.Helper()
	dir := t.TempDir()
	runDir := filepath.Join(dir, runID.String())
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e := storeExecutor(t, pool, runDir)
	e.ArtifactsDir = dir
	runner := &envRunner{}
	e.Runner = runner
	return e, runner
}

func hasPlansEnv(specs []ContainerSpec) bool {
	for _, s := range specs {
		for _, kv := range s.Env {
			if len(kv) >= 11 && kv[:11] == "E2AI_PLANS=" {
				return true
			}
		}
	}
	return false
}

func TestPlanGateDraftOnlyNeverExecutes(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedAppRun(t, pool)
	appID := runApp(t, pool, runID)
	seedPlanRow(t, pool, appID, "checkout-flow", "draft")
	addVersion(t, pool, appID, "checkout-flow", "v1.0.1", "draft")

	e, runner := gateExecutor(t, pool, runID)
	if err := e.Work(context.Background(), &river.Job[ExecuteArgs]{
		Args: ExecuteArgs{RunID: runID.String()},
	}); err != nil {
		t.Fatalf("Work returned error: %v", err)
	}

	var status string
	if err := pool.QueryRow(context.Background(),
		`select status from runs where id = $1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
	var note *string
	if err := pool.QueryRow(context.Background(),
		`select error_note from runs where id = $1`, runID).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note == nil {
		t.Error("error_note missing for draft-only block")
	}
	if hasPlansEnv(runner.Specs) || len(runner.Specs) > 0 {
		t.Errorf("draft-only catalog executed the container: %+v", runner.Specs)
	}
	_, mode, _, _ := loadResolution(t, pool, runID)
	if mode != "draft_only" {
		t.Errorf("mode = %q, want draft_only", mode)
	}
}

func TestPlanGateChangedPlanKeepsLastApproved(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedAppRun(t, pool)
	appID := runApp(t, pool, runID)
	seedPlanRow(t, pool, appID, "checkout-flow", "approved")
	approvedV1 := addVersion(t, pool, appID, "checkout-flow", "v1.0.0", "approved")
	approveVersion(t, pool, approvedV1)
	addVersion(t, pool, appID, "checkout-flow", "v1.0.1", "pending_approval")

	e, runner := gateExecutor(t, pool, runID)
	if err := e.Work(context.Background(), &river.Job[ExecuteArgs]{
		Args: ExecuteArgs{RunID: runID.String()},
	}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	_, mode, selectedRaw, _ := loadResolution(t, pool, runID)
	if mode != "approved" {
		t.Errorf("mode = %q, want approved", mode)
	}
	var rec resolutionRecord
	if err := json.Unmarshal(selectedRaw, &rec.Selected); err != nil {
		t.Fatal(err)
	}
	if len(rec.Selected) != 1 || rec.Selected[0].Version != "v1.0.0" {
		t.Errorf("selected = %+v, want v1.0.0 running", rec.Selected)
	}
	if !hasPlansEnv(runner.Specs) {
		t.Error("container did not receive E2AI_PLANS")
	}
	for _, s := range runner.Specs {
		for _, kv := range s.Env {
			if kv == "E2AI_PLANS=checkout-flow@v1.0.0@hash-v1.0.0" {
				return
			}
		}
	}
	t.Errorf("E2AI_PLANS does not carry the approved version: %+v", runner.Specs)
}

func TestPlanGateNoPlansLegacyPath(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedAppRun(t, pool)

	e, runner := gateExecutor(t, pool, runID)
	if err := e.Work(context.Background(), &river.Job[ExecuteArgs]{
		Args: ExecuteArgs{RunID: runID.String()},
	}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	_, mode, selectedRaw, _ := loadResolution(t, pool, runID)
	if mode != "legacy_full_suite" {
		t.Errorf("mode = %q, want legacy_full_suite", mode)
	}
	var selected []selectedPlan
	if err := json.Unmarshal(selectedRaw, &selected); err != nil || len(selected) != 0 {
		t.Errorf("legacy selection = %s (err %v), want empty", selectedRaw, err)
	}
	if hasPlansEnv(runner.Specs) {
		t.Error("legacy run must not receive E2AI_PLANS")
	}
	var status string
	if err := pool.QueryRow(context.Background(),
		`select status from runs where id = $1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	// The executor hands off to the report job, which owns the final
	// transition; after Work the run sits in executing.
	if status != "executing" {
		t.Errorf("status = %q, want executing (report handoff owns completion)", status)
	}
}

func TestPlanGateResolutionRecordRoundTrip(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedAppRun(t, pool)
	appID := runApp(t, pool, runID)
	seedPlanRow(t, pool, appID, "checkout-flow", "approved")
	v1 := addVersion(t, pool, appID, "checkout-flow", "v1.0.0", "approved")
	approveVersion(t, pool, v1)
	addVersion(t, pool, appID, "checkout-flow", "v1.0.1", "pending_approval")

	e, _ := gateExecutor(t, pool, runID)
	if err := e.Work(context.Background(), &river.Job[ExecuteArgs]{
		Args: ExecuteArgs{RunID: runID.String()},
	}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	// One record per run: a replay upserts in place, never duplicates.
	var count int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from run_plan_resolutions where run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("resolution rows = %d, want 1", count)
	}

	// Round-trip through the REST shape the endpoint serves.
	primary, mode, _, resolution := loadResolution(t, pool, runID)
	if primary == nil || *primary != "checkout-flow" || mode != "approved" {
		t.Fatalf("primary/mode wrong: %v/%v", primary, mode)
	}
	var rec resolutionRecord
	if err := json.Unmarshal(resolution, &rec); err != nil {
		t.Fatalf("resolution jsonb: %v", err)
	}
	if len(rec.Selected) != 1 || rec.Selected[0].PlanID != "checkout-flow" {
		t.Fatalf("selected = %+v", rec.Selected)
	}
	if len(rec.Excluded) == 0 {
		t.Error("changed plan not recorded as excluded")
	}
}

func TestE2AIPlansEnv(t *testing.T) {
	got := E2AIPlansEnv([]selectedPlan{
		{PlanID: "a", Version: "v1.0.0", Hash: "h1"},
		{PlanID: "b", Version: "v2.0.0", Hash: "h2"},
	})
	want := "a@v1.0.0@h1,b@v2.0.0@h2"
	if got != want {
		t.Errorf("E2AIPlansEnv = %q, want %q", got, want)
	}
}
