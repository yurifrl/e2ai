// Agent-session queue worker tests (Phase 3 T4): the full spawn→exit
// mapping (completed/killed/budget_exhausted/failed) plus the precondition
// gate — an unapproved plan fails the session before any child spawns.
package agents

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

func newTestWorker(pool *pgxpool.Pool) *AgentSessionWorker {
	return &AgentSessionWorker{
		Pool:     pool,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Command:  []string{"true"},
		WorkerID: "worker-test",
	}
}

func runWorker(t *testing.T, w *AgentSessionWorker, sessionID pgtype.UUID) {
	t.Helper()
	if err := w.Work(context.Background(), &river.Job[AgentSessionArgs]{
		Args: AgentSessionArgs{SessionID: sessionID.String()},
	}); err != nil {
		t.Fatalf("Work: %v", err)
	}
}

func markRunExecuting(t *testing.T, pool *pgxpool.Pool, runID pgtype.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`update runs set status = 'executing' where id = $1`, runID); err != nil {
		t.Fatalf("mark run executing: %v", err)
	}
}

// Exit mapping: the child's stderr marker + exit code decide the terminal
// status, exactly per the T5 contract.
func TestSpawnExitMapping(t *testing.T) {
	cases := []struct {
		name       string
		script     string
		wantStatus Status
		wantNote   string // substring of error_note
	}{
		{
			name:       "clean exit completes",
			script:     "exit 0",
			wantStatus: StatusCompleted,
		},
		{
			name:       "budget marker maps to budget_exhausted",
			script:     "echo 'BudgetExhausted: cost ceiling' >&2; exit 3",
			wantStatus: StatusBudgetExhausted,
			wantNote:   "BudgetExhausted",
		},
		{
			name:       "killed marker maps to killed",
			script:     "echo 'KilledError: kill switch' >&2; exit 7",
			wantStatus: StatusKilled,
			wantNote:   "KilledError",
		},
		{
			name:       "plain failure keeps stderr tail",
			script:     "echo 'model gateway 503' >&2; exit 9",
			wantStatus: StatusFailed,
			wantNote:   "model gateway 503",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			runID := seedRun(t, pool)
			markRunExecuting(t, pool, runID)
			sessionID := seedSession(t, pool, runID, RoleNavigator, 10, 100_000)

			w := newTestWorker(pool)
			w.Command = []string{"sh", "-c", tc.script}
			runWorker(t, w, sessionID)

			status, note, worker, _, _ := sessionRow(t, pool, sessionID)
			if status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (note: %s)", status, tc.wantStatus, note)
			}
			if tc.wantNote != "" && !strings.Contains(note, tc.wantNote) {
				t.Fatalf("error_note %q missing marker %q", note, tc.wantNote)
			}
			if worker == nil || *worker != "worker-test" {
				t.Fatalf("worker_id = %v, want worker-test", worker)
			}
		})
	}
}

// Spawn contract: the child receives the full env wiring — session id, role,
// ceilings, API/gateway config — as exported vars, plus --session/--role
// argv. Proven by having the fake agentrunner dump what it saw.
func TestSpawnEnvAndArgvWiring(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	markRunExecuting(t, pool, runID)
	// Non-default surface: proves the run's surface (not a hardcoded value)
	// reaches the child. Migration default covers the web case implicitly.
	if _, err := pool.Exec(context.Background(),
		`update runs set surface = 'android' where id = $1`, runID); err != nil {
		t.Fatalf("set run surface: %v", err)
	}
	sessionID := seedSession(t, pool, runID, RoleNavigator, 42, 100_000)

	out := filepath.Join(t.TempDir(), "spawn.txt")
	script := `echo "$E2AI_SESSION_ID|$E2AI_RUN_ID|$E2AI_SURFACE|$E2AI_ROLE|$E2AI_AGENT_MODEL_NAVIGATOR|$2 $3|$E2AI_API_BASE_URL|$E2AI_RUNNER_SERVICE_TOKEN|$AIHUB_BASE_URL|$AIHUB_API_TOKEN|$E2AI_ARTIFACTS_DIR|$E2AI_STEP_CEILING|$E2AI_COST_CEILING_MILLICENTS" > "$SPAWN_OUT"`
	w := newTestWorker(pool)
	w.Command = []string{"sh", "-c", script, "agentrunner"} // $0=agentrunner, $1=--session, $2=<id>, $3=--role
	w.RoleModels = map[Role]string{RoleNavigator: "glm-5.3-flash"}
	w.APIBase = "http://api:8080"
	w.ServiceToken = "svc-token"
	w.AIHUBBase = "https://aihub.example/v1"
	w.AIHUBToken = "aihub-token"
	w.ArtifactsDir = "/var/lib/e2ai/artifacts"
	t.Setenv("SPAWN_OUT", out)
	runWorker(t, w, sessionID)

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("fake agentrunner never ran (no spawn dump): %v", err)
	}
	want := sessionID.String() + "|" + runID.String() + "|android|navigator|glm-5.3-flash|" + sessionID.String() + " --role|http://api:8080|svc-token|https://aihub.example/v1|aihub-token|/var/lib/e2ai/artifacts|42|100000\n"
	if string(got) != want {
		t.Fatalf("spawn wiring:\n got %q\nwant %q", string(got), want)
	}
}

// Precondition gate: a plan-linked run whose plan never got approved fails
// the session BEFORE any child process runs. Covers both resolutions the
// gate rejects: draft_only (never approved) and approved-with-drift (the
// registry plan row left pending). Proven by a command that would dump a
// marker file if it ever spawned.
func TestPreconditionUnapprovedPlanFailsBeforeSpawn(t *testing.T) {
	for _, tc := range []struct {
		name           string
		planStatus     string
		resolutionMode string
		noteHas        string
	}{
		{"draft_only", "pending", "draft_only", "draft_only"},
		{"approved mode with pending plan row", "pending", "approved", "want approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			runID := seedRun(t, pool)
			markRunExecuting(t, pool, runID)
			sessionID := seedSession(t, pool, runID, RoleNavigator, 10, 100_000)

			var appID pgtype.UUID
			if err := pool.QueryRow(context.Background(),
				`select app_id from runs where id = $1`, runID).Scan(&appID); err != nil {
				t.Fatalf("load app id: %v", err)
			}
			if _, err := pool.Exec(context.Background(), `
				insert into plans (id, app_id, source_path, status)
				values ('check-flow', $1, 'plans/check-flow.md', $2)
			`, appID, tc.planStatus); err != nil {
				t.Fatalf("seed plan: %v", err)
			}
			if _, err := pool.Exec(context.Background(), `
				insert into run_plan_resolutions (run_id, plan_id, mode, resolution)
				values ($1, 'check-flow', $2, '{"selected":[]}'::jsonb)
			`, runID, tc.resolutionMode); err != nil {
				t.Fatalf("seed resolution: %v", err)
			}

			dump := filepath.Join(t.TempDir(), "spawn.txt")
			w := newTestWorker(pool)
			w.Command = []string{"sh", "-c", `echo spawned > "$SPAWN_OUT"`, "agentrunner"}
			t.Setenv("SPAWN_OUT", dump)
			runWorker(t, w, sessionID)

			status, note, worker, _, _ := sessionRow(t, pool, sessionID)
			if status != StatusFailed {
				t.Fatalf("status = %q, want failed (note %q)", status, note)
			}
			if !strings.Contains(note, tc.noteHas) {
				t.Fatalf("error_note %q should name the precondition (%q)", note, tc.noteHas)
			}
			if worker == nil || *worker != "worker-test" {
				t.Fatalf("worker_id = %v, want worker-test", worker)
			}
			if _, err := os.Stat(dump); !os.IsNotExist(err) {
				t.Fatal("agentrunner child spawned despite precondition failure")
			}
		})
	}
}
