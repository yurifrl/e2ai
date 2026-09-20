package runs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Run mirrors one runs row (pipeline state, T5 ingest / T8 worker write it,
// these queries read it). Nullable columns use pgtype value types.
type Run struct {
	ID            pgtype.UUID
	AppID         pgtype.UUID
	WorkflowRunID int64
	RunNumber     int32
	HeadSHA       string
	HeadBranch    pgtype.Text
	Environment   string
	TriggerSource string
	Status        Status
	Verdict       pgtype.Text
	PRNumber      pgtype.Int4
	PRState       pgtype.Text
	TotalTests    pgtype.Int4
	Passed        pgtype.Int4
	Failed        pgtype.Int4
	Flaky         pgtype.Int4
	Skipped       pgtype.Int4
	DurationMS    pgtype.Int4
	ReportURL     pgtype.Text
	CommentURL    pgtype.Text
	CheckRunID    pgtype.Int8
	ErrorNote     pgtype.Text
	StartedAt     pgtype.Timestamptz
	FinishedAt    pgtype.Timestamptz
	CreatedAt     pgtype.Timestamptz
}

const runColumns = `
	id, app_id, workflow_run_id, run_number, head_sha, head_branch, environment,
	trigger_source, status, verdict, pr_number, pr_state,
	total_tests, passed, failed, flaky, skipped, duration_ms,
	report_url, comment_url, check_run_id, error_note, started_at, finished_at, created_at`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.AppID, &r.WorkflowRunID, &r.RunNumber, &r.HeadSHA, &r.HeadBranch,
		&r.Environment, &r.TriggerSource, &r.Status, &r.Verdict, &r.PRNumber, &r.PRState,
		&r.TotalTests, &r.Passed, &r.Failed, &r.Flaky, &r.Skipped, &r.DurationMS,
		&r.ReportURL, &r.CommentURL, &r.CheckRunID, &r.ErrorNote, &r.StartedAt, &r.FinishedAt, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan run: %w", err)
	}
	return &r, nil
}

// Page bounds a list. Limit defaults to 50 and is capped at 200; callers get
// the applied values back via the returned slice's length and the cap.
type Page struct {
	Limit  int
	Offset int
}

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

func (p Page) bounds() (int, int) {
	limit := p.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	offset := p.Offset
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// ListRuns returns runs newest first, paginated by offset. Pagination is
// stable across pages because created_at desc is tie-broken by id desc.
// A null appID lists across every app (the /api/runs page surface); a valid
// one scopes the page to that app.
func ListRuns(ctx context.Context, db DB, appID pgtype.UUID, page Page) ([]Run, error) {
	limit, offset := page.bounds()
	rows, err := db.Query(ctx, `
		select `+runColumns+`
		from runs
		where ($1::uuid is null or app_id = $1)
		order by created_at desc, id desc
		limit $2 offset $3
	`, appID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var runs []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runs: %w", err)
	}
	return runs, nil
}

// GetRun returns the run or nil when it does not exist.
func GetRun(ctx context.Context, db DB, id pgtype.UUID) (*Run, error) {
	r, err := scanRun(db.QueryRow(ctx, `
		select `+runColumns+`
		from runs
		where id = $1
	`, id))
	if err != nil {
		return nil, err
	}
	return r, nil
}

// TestResult mirrors one test_results row: per-test Playwright evidence
// (VERD-01/REPT-01) — outcome is the Playwright top-level status enum, status
// the last-attempt status, attachments the artifact pointers as stored.
type TestResult struct {
	ID             pgtype.UUID
	RunID          pgtype.UUID
	Suite          pgtype.Text
	Title          string
	File           pgtype.Text
	Line           pgtype.Int4
	Project        pgtype.Text
	Outcome        string
	Status         string
	Attempts       int32
	DurationMS     pgtype.Int4
	ErrorMessage   pgtype.Text
	Classification pgtype.Text
	Attachments    []byte // raw jsonb; consumers decode the [{kind, path}] array
}

// Artifact mirrors one artifacts row (REPT-01: report serving).
type Artifact struct {
	ID        pgtype.UUID
	RunID     pgtype.UUID
	Kind      string
	Path      string
	SizeBytes pgtype.Int8
	CreatedAt pgtype.Timestamptz
}

// RunDetail is the full run surface: the row plus its per-test evidence and
// artifact index.
type RunDetail struct {
	Run         *Run
	TestResults []TestResult
	Artifacts   []Artifact
}

// GetRunDetail loads the run, its test_results (suite/file order for a stable
// rendering) and artifacts (newest first) in one call. Run is nil when the
// run does not exist.
func GetRunDetail(ctx context.Context, db DB, id pgtype.UUID) (*RunDetail, error) {
	run, err := GetRun(ctx, db, id)
	if err != nil || run == nil {
		return nil, err
	}
	detail := &RunDetail{Run: run}

	rows, err := db.Query(ctx, `
		select id, run_id, suite, title, file, line, project, outcome, status,
		       attempts, duration_ms, error_message, classification, attachments
		from test_results
		where run_id = $1
		order by coalesce(suite, ''), coalesce(file, ''), title
	`, id)
	if err != nil {
		return nil, fmt.Errorf("list test results: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tr TestResult
		if err := rows.Scan(&tr.ID, &tr.RunID, &tr.Suite, &tr.Title, &tr.File, &tr.Line,
			&tr.Project, &tr.Outcome, &tr.Status, &tr.Attempts, &tr.DurationMS,
			&tr.ErrorMessage, &tr.Classification, &tr.Attachments); err != nil {
			return nil, fmt.Errorf("scan test result: %w", err)
		}
		detail.TestResults = append(detail.TestResults, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate test results: %w", err)
	}

	rows, err = db.Query(ctx, `
		select id, run_id, kind, path, size_bytes, created_at
		from artifacts
		where run_id = $1
		order by created_at desc, id desc
	`, id)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.RunID, &a.Kind, &a.Path, &a.SizeBytes, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		detail.Artifacts = append(detail.Artifacts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate artifacts: %w", err)
	}
	return detail, nil
}
