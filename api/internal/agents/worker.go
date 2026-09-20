// Agent-session queue worker (Phase 3 T4): the river "agent" queue job that
// turns a queued session into a live agentrunner child process. ARCHITECTURE
// Pattern 1 — the process is ephemeral and disposable; every durable fact
// (status, steps, cost) lives in Postgres and travels over the HTTP API, so
// losing the child costs nothing but the compute.
//
// Spawn contract (RESEARCH.md §orchestrator): `node <bundle> --session <id>
// --role <role>` with the session identity, API base + service token, gateway
// base + token ref, artifacts dir, and the per-role model + ceiling budget in
// the environment. Child stderr streams into the runner log; the tail is kept
// as the failure evidence. Exit mapping: 0 → completed, stderr
// BudgetExhausted → budget_exhausted, stderr KilledError → killed, anything
// else → failed. MaxAttempts 1: agent sessions are human-observable — a retry
// would duplicate a visible agent, so failures finalize on the first attempt
// (Work always returns nil).
package agents

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// AgentSessionArgs carries the session the worker must drive. The worker
// loads everything else (run, role, ceilings) from Postgres.
type AgentSessionArgs struct {
	SessionID string
}

// Kind implements river.JobArgs.
func (AgentSessionArgs) Kind() string { return "e2ai:agent:session" }

// Stderr markers the agentrunner bundle prints before a non-zero exit (T5
// contract). BudgetExhausted is checked first: a budget breach inside a
// killed child must not mask the real terminal cause.
const (
	StderrBudgetMarker = "BudgetExhausted"
	StderrKilledMarker = "KilledError"
)

// runHealthyStatuses are the run states under which agent sessions may spawn:
// the rollout gate has passed (provisioning/executing) and reporting is still
// finalizing. queued/waiting-rollout/failed/completed runs must not spawn.
var runHealthyStatuses = map[string]bool{
	"provisioning": true,
	"executing":    true,
	"reporting":    true,
}

// AgentSessionWorker spawns and supervises one agentrunner child per job.
// Command is the spawn prefix, e.g. []string{"node", "/app/agentrunner.mjs"} —
// the worker appends --session/--role. ServiceToken arrives already resolved
// from its op:// ref via .env (SECR-01: `op inject` renders it, never Go).
type AgentSessionWorker struct {
	river.WorkerDefaults[AgentSessionArgs]

	Pool   *pgxpool.Pool
	Logger *slog.Logger

	Command []string

	APIBase       string
	ServiceToken  string
	AIHUBBase     string
	AIHUBToken    string
	ArtifactsDir  string
	RoleModels    map[Role]string
	WorkerID      string
}

// Work drives the session. It always returns nil: every outcome lands in the
// session row (completed/failed/killed/budget_exhausted), so a river retry
// could only duplicate a visible agent.
func (w *AgentSessionWorker) Work(ctx context.Context, job *river.Job[AgentSessionArgs]) error {
	var id pgtype.UUID
	if err := id.Scan(job.Args.SessionID); err != nil {
		return fmt.Errorf("parse session id %q: %w", job.Args.SessionID, err)
	}
	w.runSession(ctx, id)
	return nil
}

// runSession executes the whole lifecycle for one session id. Errors do not
// propagate: the session's own status is the record of what happened.
func (w *AgentSessionWorker) runSession(ctx context.Context, id pgtype.UUID) {
	session, err := w.loadSession(id)
	if err != nil {
		w.Logger.Error("agent session worker: load session", "session_id", id, "error", err)
		return
	}
	if session.status.Terminal() {
		w.Logger.Info("agent session worker: session already terminal", "session_id", id, "status", session.status)
		return
	}
	if err := Transition(ctx, w.Pool, id, StatusClaimed); err != nil {
		// queued → claimed lost a race with the sweep; nothing to spawn.
		w.Logger.Info("agent session worker: claim lost", "session_id", id, "error", err)
		return
	}
	if _, err := w.Pool.Exec(ctx,
		`update agent_sessions set worker_id = $1, claimed_at = now(), heartbeat_at = now() where id = $2`,
		w.WorkerID, id); err != nil {
		w.Logger.Error("agent session worker: stamp worker", "session_id", id, "error", err)
	}

	if note, ok := w.checkPreconditions(ctx, id, session); !ok {
		w.Logger.Warn("agent session worker: precondition failed", "session_id", id, "reason", note)
		w.finish(ctx, id, StatusFailed, "precondition: "+note)
		return
	}

	if err := Transition(ctx, w.Pool, id, StatusRunning); err != nil {
		w.Logger.Info("agent session worker: start lost", "session_id", id, "error", err)
		return
	}
	if err := Heartbeat(ctx, w.Pool, id); err != nil {
		w.Logger.Error("agent session worker: heartbeat", "session_id", id, "error", err)
	}

	status, note := w.spawn(ctx, id, session)
	w.finish(ctx, id, status, note)
}

// sessionRuntime is the flattened session + run state the worker needs.
type sessionRuntime struct {
	status      Status
	role        Role
	runID       pgtype.UUID
	stepCeiling int
	costCeiling int64
	runStatus   string
}

func (w *AgentSessionWorker) loadSession(id pgtype.UUID) (sessionRuntime, error) {
	var s sessionRuntime
	err := w.Pool.QueryRow(context.Background(), `
		select s.status, s.role, s.run_id, s.step_ceiling, s.cost_ceiling_millicents, r.status
		from agent_sessions s join runs r on r.id = s.run_id
		where s.id = $1
	`, id).Scan(&s.status, &s.role, &s.runID, &s.stepCeiling, &s.costCeiling, &s.runStatus)
	if err != nil {
		return s, fmt.Errorf("load agent session %s: %w", id, err)
	}
	return s, nil
}

// checkPreconditions verifies the run gate and, when the run is plan-linked,
// that the deployed plan is approved. Returns "" when clear, otherwise the
// reason recorded on the failed session.
func (w *AgentSessionWorker) checkPreconditions(ctx context.Context, id pgtype.UUID, s sessionRuntime) (string, bool) {
	if !runHealthyStatuses[s.runStatus] {
		return fmt.Sprintf("run status %q is not rollout-healthy (want provisioning|executing|reporting)", s.runStatus), false
	}

	var (
		planID    pgtype.Text
		versionID pgtype.UUID
		mode      string
	)
	err := w.Pool.QueryRow(ctx, `
		select plan_id, plan_version_id, mode from run_plan_resolutions where run_id = $1
	`, s.runID).Scan(&planID, &versionID, &mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", true // no resolution row: legacy run predating the gate, nothing plan-linked to enforce
	}
	if err != nil {
		return fmt.Sprintf("load plan resolution: %v", err), false
	}
	switch mode {
	case "legacy_full_suite":
		return "", true
	case "draft_only":
		return "run is plan-linked but resolution is draft_only (plan never approved)", false
	}
	// mode == approved: the resolution gate already picked approved versions;
	// the plans row is the cross-check for anything that mutated since.
	var planStatus string
	if err := w.Pool.QueryRow(ctx,
		`select status from plans where id = $1`, planID).Scan(&planStatus); err != nil {
		return fmt.Sprintf("plan %s missing from registry: %v", planID.String, err), false
	}
	if planStatus != "approved" {
		return fmt.Sprintf("plan %s status is %q, want approved", planID.String, planStatus), false
	}
	return "", true
}

// spawn runs the agentrunner child and maps its exit onto the session state
// machine. Stderr streams into the runner log and its tail becomes the
// failure note.
func (w *AgentSessionWorker) spawn(ctx context.Context, id pgtype.UUID, s sessionRuntime) (Status, string) {
	cmdCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, w.Command[0], append(append([]string{}, w.Command[1:]...),
		"--session", id.String(), "--role", string(s.role))...)
	// Env names are the agentrunner contract (agentrunner/src/index.ts) —
	// TS is the consumer and owns the names; keep in sync with it.
	env := []string{
		"E2AI_SESSION_ID=" + id.String(),
		"E2AI_RUN_ID=" + s.runID.String(),
		"E2AI_ROLE=" + string(s.role),
		"E2AI_API_BASE_URL=" + w.APIBase,
		"E2AI_RUNNER_SERVICE_TOKEN=" + w.ServiceToken,
		"AIHUB_BASE_URL=" + w.AIHUBBase,
		"AIHUB_API_TOKEN=" + w.AIHUBToken,
		"E2AI_ARTIFACTS_DIR=" + w.ArtifactsDir,
		fmt.Sprintf("E2AI_STEP_CEILING=%d", s.stepCeiling),
		fmt.Sprintf("E2AI_COST_CEILING_MILLICENTS=%d", s.costCeiling),
	}
	for role, model := range w.RoleModels {
		env = append(env, "E2AI_AGENT_MODEL_"+strings.ToUpper(string(role))+"="+model)
	}
	cmd.Env = append(os.Environ(), env...)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return StatusFailed, fmt.Sprintf("stderr pipe: %v", err)
	}
	w.Logger.Info("agent session worker: spawning agentrunner", "role", s.role, "session", id.String())
	if err := cmd.Start(); err != nil {
		return StatusFailed, fmt.Sprintf("spawn agentrunner: %v", err)
	}
	tail := streamStderr(w.Logger, stderr)
	waitErr := cmd.Wait()

	if waitErr == nil {
		return StatusCompleted, ""
	}
	if ctx.Err() != nil {
		return StatusKilled, "runner shutting down: agentrunner child terminated"
	}
	switch {
	case strings.Contains(tail, StderrBudgetMarker):
		return StatusBudgetExhausted, "agentrunner budget breach (BudgetExhausted)"
	case strings.Contains(tail, StderrKilledMarker):
		return StatusKilled, "agentrunner killed (KilledError)"
	default:
		return StatusFailed, "agentrunner exit " + waitErr.Error() + " | stderr: " + tail
	}
}

// streamStderr copies the child's stderr into the runner log line by line and
// keeps the last few lines as the failure evidence tail.
func streamStderr(logger *slog.Logger, r io.Reader) string {
	const tailLines = 8
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		logger.Warn("agentrunner stderr", "line", line)
		lines = append(lines, line)
		if len(lines) > tailLines {
			lines = lines[len(lines)-tailLines:]
		}
	}
	return strings.Join(lines, "\n")
}

// finish applies a terminal transition and records the note. A rejection is
// expected when someone else finalized first (kill switch 409, budget engine
// breach): the row's own status is authoritative, so we just log.
func (w *AgentSessionWorker) finish(ctx context.Context, id pgtype.UUID, to Status, note string) {
	if err := Transition(ctx, w.Pool, id, to); err != nil {
		w.Logger.Info("agent session worker: final transition rejected", "session_id", id, "to", to, "error", err)
		return
	}
	if note == "" {
		return
	}
	if _, err := w.Pool.Exec(ctx,
		`update agent_sessions set error_note = coalesce(error_note || ' | ', '') || $1 where id = $2 and status = $3`,
		truncateNote(note, 500), id, to); err != nil {
		w.Logger.Error("agent session worker: record note", "session_id", id, "error", err)
	}
}

func truncateNote(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
