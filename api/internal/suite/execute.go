// Execute job (SUIT-01/02, REPT-03 groundwork): the runner container's
// worker for kind e2ai:run:execute. One job = one run:
//
//	provisioning → executing (guarded transition, T7's gate enqueued us there)
//	→ neutral check-run (in_progress) on the run's head SHA
//	→ one docker run: clone head_sha → corepack/yarn build → serve :3000
//	  → npx playwright test --reporter=json → results.json + attachments
//	→ parse the report, store test_results + artifact rows (one tx)
//	→ enqueue the report job (e2ai:run:report, default queue) and exit
//
// Verdicts and run counters are the report job's business (T9) — this stage
// only lays down evidence rows, every verdict traceable to them (VERD-01).
package suite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/NSXBet/e2ai/api/internal/runs"
	"github.com/NSXBet/e2ai/api/internal/verdict"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// pgUUID maps the google uuid job args onto runs.Transition's pgtype.UUID.
func pgUUID(id uuid.UUID) pgtype.UUID {
	u := pgtype.UUID{Bytes: id, Valid: true}
	return u
}

const (
	// JobKindExecute is this package's runner-queue job kind.
	JobKindExecute = "e2ai:run:execute"

	// JobKindReport is the report handoff kind (worked by internal/report,
	// default queue). Enqueue contract with the report worker: payload
	// {"run_id": "<uuid string>"}, same field name as ExecuteArgs.
	JobKindReport = "e2ai:run:report"
)

// ExecuteArgs is the execute job's payload: the run to execute.
type ExecuteArgs struct {
	RunID string `json:"run_id"` // uuid string
}

func (ExecuteArgs) Kind() string { return JobKindExecute }

// reportArgs is the report handoff payload (defined here until the report
// package owns it; the runner never inserts it — the api does).
type reportArgs struct {
	RunID string `json:"run_id"`
}

func (reportArgs) Kind() string { return JobKindReport }

// reportOpts is the report enqueue: default queue (api drains it), one shot.
func reportOpts() *river.InsertOpts {
	return &river.InsertOpts{
		Queue:       river.QueueDefault,
		MaxAttempts: 1,
		UniqueOpts: river.UniqueOpts{ByState: []rivertype.JobState{
			rivertype.JobStateScheduled,
			rivertype.JobStatePending,
			rivertype.JobStateAvailable,
			rivertype.JobStateRunning,
		}},
	}
}

// JobEnqueuer is the insert surface the executor needs; satisfied directly
// by river.Client's non-generic Insert.
type JobEnqueuer interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Mount is one host-path bind into the container.
type Mount struct {
	Host      string
	Container string
}

// ContainerSpec describes one suite execution: image to run, env, binds.
type ContainerSpec struct {
	Image  string
	Env    []string
	Mounts []Mount
}

// ContainerRunner executes one suite container. The docker implementation
// shells out to the docker CLI; tests and T13 substitute fakes.
type ContainerRunner interface {
	Run(ctx context.Context, spec ContainerSpec) error
}

// DockerRunner runs suites via `docker run`. Env plumbing is explicit — the
// container gets only what the spec lists (SECR-01: secrets never leak into
// the suite's environment).
type DockerRunner struct{}

// Run starts the suite container and blocks until it exits.
func (DockerRunner) Run(ctx context.Context, spec ContainerSpec) error {
	args := []string{"run", "--rm"}
	for _, m := range spec.Mounts {
		args = append(args, "-v", m.Host+":"+m.Container)
	}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	args = append(args, spec.Image)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("suite container %s: %w\n%s", spec.Image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Executor is the execute job worker.
type Executor struct {
	river.WorkerDefaults[ExecuteArgs]

	// Pool is the postgres pool (suite + artifacts rows, run transitions).
	Pool *pgxpool.Pool
	// Queue enqueues the report handoff.
	Queue JobEnqueuer
	// Runner executes the suite container.
	Runner ContainerRunner
	// Image is the runner image (default e2ai-runner:local).
	Image string
	// ArtifactsDir is the host directory bound into every suite container;
	// per-run results land at ArtifactsDir/<run id>/.
	ArtifactsDir string
	// CheckRuns creates the neutral in_progress check-run; nil disables
	// (tests, no-token environments — the check run is reporting sugar, not
	// evidence).
	CheckRuns CheckRunCreator

	// GitHubToken is handed to the suite container so its entrypoint can
	// clone the private checkout at the run's head SHA. Empty skips the env
	// (public repos, tests).
	GitHubToken string
	// Logger receives stage transitions; nil uses slog.Default().
	Logger *slog.Logger
}

// CheckRunCreator is the GitHub surface the executor consumes.
type CheckRunCreator interface {
	CreateCheckRun(ctx context.Context, owner, repo string, in CheckRunInput) (*CheckRun, error)
}

// CheckRunInput/CheckRun mirror the github client's shapes without importing
// the package (the runner binary shares them via a small alias below).
type CheckRunInput struct {
	Name    string
	HeadSHA string
	Status  string
}

type CheckRun struct {
	ID int64
}

// Work runs the execute job.
func (e *Executor) Work(ctx context.Context, job *river.Job[ExecuteArgs]) error {
	runID, err := uuid.Parse(job.Args.RunID)
	if err != nil {
		return fmt.Errorf("parse run id %q: %w", job.Args.RunID, err)
	}
	logger := e.logger()
	logger = logger.With("run_id", runID.String())

	run, err := e.loadRun(ctx, runID)
	if err != nil {
		return err
	}

	if err := e.transition(ctx, run, "provisioning → executing"); err != nil {
		return err
	}

	// Neutral check-run in_progress on the head SHA (REPT-03 groundwork).
	if e.CheckRuns != nil {
		cr, err := e.CheckRuns.CreateCheckRun(ctx, run.owner, run.repo, CheckRunInput{
			Name:    "e2ai",
			HeadSHA: run.headSHA,
			Status:  "in_progress",
		})
		if err != nil {
			// Non-fatal: the suite still runs; GitHub check status is sugar.
			logger.Warn("check-run create failed; continuing", "error", err)
		} else if _, err := e.Pool.Exec(ctx, `update runs set check_run_id = $1 where id = $2`, cr.ID, runID); err != nil {
			logger.Warn("check_run_id persist failed; continuing", "error", err)
		}
	}

	artifactsDir := filepath.Join(e.ArtifactsDir, runID.String())
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		return e.fail(ctx, run, "provisioning", err, logger)
	}

	logger.Info("running suite container", "image", e.Image, "sha", run.headSHA)
	env := []string{
		"E2AI_REPO=" + run.owner + "/" + run.repo,
		"E2AI_SHA=" + run.headSHA,
		"E2AI_ARTIFACTS_DIR=/artifacts",
	}
	if e.GitHubToken != "" {
		env = append(env, "E2AI_GITHUB_TOKEN="+e.GitHubToken)
	}
	err = e.Runner.Run(ctx, ContainerSpec{
		Image:  e.Image,
		Env:    env,
		Mounts: []Mount{{Host: artifactsDir, Container: "/artifacts"}},
	})
	if err != nil {
		return e.fail(ctx, run, "container", err, logger)
	}
	data, err := os.ReadFile(filepath.Join(artifactsDir, "results.json"))
	if err != nil {
		return e.fail(ctx, run, "parse", err, logger)
	}
	rep, err := Parse(data)
	if err != nil {
		return e.fail(ctx, run, "parse", err, logger)
	}

	if err := e.store(ctx, runID, rep, artifactsDir); err != nil {
		return e.fail(ctx, run, "persist", err, logger)
	}
	if _, err := e.Queue.Insert(ctx, reportArgs{RunID: runID.String()}, reportOpts()); err != nil {
		return e.fail(ctx, run, "report enqueue", err, logger)
	}

	logger.Info("suite executed; report enqueued",
		"tests", len(rep.Tests), "stats", rep.Stats)
	return nil
}

// suiteRun is the run + app fields the executor reads.
type suiteRun struct {
	id      uuid.UUID
	owner   string
	repo    string
	headSHA string
	status  runs.Status
}

// loadRun fetches the run joined with its registered app.
func (e *Executor) loadRun(ctx context.Context, runID uuid.UUID) (*suiteRun, error) {
	r := &suiteRun{id: runID}
	err := e.Pool.QueryRow(ctx, `
		select r.head_sha, r.status, a.owner, a.repo
		from runs r
		join registered_apps a on a.id = r.app_id
		where r.id = $1
	`, runID).Scan(&r.headSHA, &r.status, &r.owner, &r.repo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("run %s not found", runID)
	}
	if err != nil {
		return nil, fmt.Errorf("load run %s: %w", runID, err)
	}
	return r, nil
}

// transition performs one guarded stage move; a concurrent worker that
// already made the move is tolerated (idempotent replay), a lost race or
// illegal move is a typed error.
func (e *Executor) transition(ctx context.Context, r *suiteRun, stage string) error {
	err := runs.Transition(ctx, e.Pool, pgUUID(r.id), runs.StatusExecuting)
	if err == nil {
		return nil
	}
	var terr *runs.TransitionError
	if errors.As(err, &terr) && terr.From == runs.StatusExecuting && terr.To == runs.StatusExecuting {
		return nil // replayed job: the stage already moved
	}
	return fmt.Errorf("%s: %w", stage, err)
}

// fail transitions the run to failed with a stage-tagged note and logs.
// Transition failures here (e.g. run already failed by a concurrent replay)
// are logged, not returned — the underlying job error takes precedence.
func (e *Executor) fail(ctx context.Context, r *suiteRun, stage string, cause error, logger *slog.Logger) error {
	_ = errors.Join(cause, noteFail(ctx, e.Pool, r.id, stage, cause))
	logger.Error("suite execution failed", "stage", stage, "error", cause)
	return fmt.Errorf("execute %s: %w", stage, cause)
}

// store persists the parsed report: test_results (with per-test
// classification from the verdict engine's deterministic heuristics) and
// artifact rows, one transaction. Attachments walk the run's artifacts
// directory so paths stay volume-relative and traversal-safe (SECR-01).
func (e *Executor) store(ctx context.Context, runID uuid.UUID, rep *Report, artifactsDir string) error {
	tx, err := e.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin store tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Replay safety: River redelivery (worker crash between commit and ack)
	// re-runs the job. Clearing this run's evidence inside the same tx makes
	// attempt 2 converge on attempt 1's rows instead of duplicating them.
	if _, err := tx.Exec(ctx, `delete from artifacts where run_id = $1`, runID); err != nil {
		return fmt.Errorf("clear stale artifacts: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from test_results where run_id = $1`, runID); err != nil {
		return fmt.Errorf("clear stale test results: %w", err)
	}

	for _, t := range rep.Tests {
		class := verdict.Classify(t.Evidence())
		atts, err := attachmentJSON(t.Attachments)
		if err != nil {
			return err
		}
		var dur any
		if t.Attempts > 0 {
			dur = t.DurationMS
		}
		if _, err := tx.Exec(ctx, `
			insert into test_results
				(run_id, suite, title, file, line, project, outcome, status,
				 attempts, duration_ms, error_message, classification, attachments)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, runID, t.Suite, t.Title, t.File, t.Line, t.Project, t.Outcome, t.Status,
			t.Attempts, dur, nullIfEmpty(t.ErrorMessage), string(class), atts); err != nil {
			return fmt.Errorf("insert test result %s: %w", t.ID, err)
		}
	}

	if err := insertArtifacts(ctx, tx, runID, artifactsDir); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// insertArtifacts walks the run's artifacts directory and records one row per
// file, kind inferred from the name. Paths are stored volume-relative
// ("results.json", "screenshots/x.png") and served under /e2ai/artifacts/.
func insertArtifacts(ctx context.Context, tx pgx.Tx, runID uuid.UUID, dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk artifacts: %w", err)
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("artifact rel path: %w", err)
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("artifact stat %s: %w", rel, err)
		}
		if _, err := tx.Exec(ctx, `
			insert into artifacts (run_id, kind, path, size_bytes)
			values ($1, $2, $3, $4)
		`, runID, artifactKind(rel), rel, info.Size()); err != nil {
			return fmt.Errorf("insert artifact %s: %w", rel, err)
		}
		return nil
	})
}

// artifactKind maps a file name/path to the artifacts.kind enum-ish values
// the schema documents (html_report|blob|trace|screenshot|video|log|results_json).
func artifactKind(path string) string {
	name := strings.ToLower(filepath.Base(path))
	switch {
	case name == "results.json":
		return "results_json"
	case strings.Contains(name, "trace"):
		return "trace"
	case strings.Contains(name, "screenshot"):
		return "screenshot"
	case strings.HasSuffix(name, ".webm"):
		return "video"
	case strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".txt"):
		return "log"
	case strings.HasSuffix(name, ".zip"):
		return "blob"
	default:
		return "log"
	}
}

// attachmentJSON encodes one test's attachments as the jsonb array the
// schema documents: [{kind, path}] with kind screenshot|trace|video (other
// content types are skipped — only verdict-relevant evidence is recorded).
func attachmentJSON(atts []Attachment) ([]byte, error) {
	type att struct {
		Kind string `json:"kind"`
		Path string `json:"path"`
	}
	var rows []att
	for _, a := range atts {
		var kind string
		switch {
		case strings.Contains(strings.ToLower(a.Name), "screenshot"):
			kind = "screenshot"
		case strings.Contains(strings.ToLower(a.Name), "trace"):
			kind = "trace"
		case strings.Contains(strings.ToLower(a.Name), "video"):
			kind = "video"
		default:
			continue
		}
		rows = append(rows, att{Kind: kind, Path: a.Path})
	}
	if rows == nil {
		return nil, nil
	}
	return json.Marshal(rows)
}

// noteFail stamps runs.status = failed and error_note in one guarded update
// (executing → failed is legal from every active stage, including
// provisioning). Returns the transition/persist error for Join.
func noteFail(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID, stage string, cause error) error {
	if err := runs.Transition(ctx, pool, pgUUID(runID), runs.StatusFailed); err != nil {
		return fmt.Errorf("mark run failed: %w", err)
	}
	if _, err := pool.Exec(ctx, `update runs set error_note = $1 where id = $2`,
		fmt.Sprintf("%s: %v", stage, cause), runID); err != nil {
		return fmt.Errorf("write error note: %w", err)
	}
	return nil
}

func (e *Executor) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
