// T10: the ReportPublisher implementation — sticky PR comment + signed
// report-page link, mounted on the job's Publisher seam after the verdict
// is committed (REPT-01/02/03). Report-only: the publisher adds sugar on
// top of a completed run and never re-fails it.
package report

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NSXBet/e2ai/api/internal/platform/github"
	"github.com/NSXBet/e2ai/api/internal/runs"
	"github.com/NSXBet/e2ai/api/internal/verdict"
)

// publish constants: rate budget floor (skip publishing when at/below —
// observational, never a retry loop; the next run republishes), the cap on
// the "top failures" section, and one screenshot per failing test so the
// sticky body stays readable.
const (
	budgetFloor = 5
	maxFailures = 5
)

// Publisher is the T10 ReportPublisher: it resolves the run's PR by head
// SHA, upserts the app's sticky comment (create once, then PATCH the same
// comment via the marker scan), patches the run's check-run to completed
// with an ALWAYS-neutral conclusion, and persists the produced URLs.
//
// Secrets and origins arrive via env (SECR-01): reportBase is the public
// origin of the report page, linkSecret signs the run id for it.
type Publisher struct {
	gh         *github.Client
	reportBase string
	linkSecret string
	logger     *slog.Logger

	// load assembles the per-run publish inputs. Default impl reads
	// Postgres (loadFacts); tests mount an in-memory loader to exercise
	// the full flow against a fake GitHub without a database.
	load loadFunc
	// record persists the produced URLs + sticky-comment state. Default
	// impl writes Postgres (recordURLs); tests mount a recorder.
	record recordFunc
}

// loadFunc assembles one run's publish inputs.
type loadFunc func(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID, rep verdict.Report) (*runFacts, error)

// recordFunc persists the publish outputs for one run.
type recordFunc func(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID, facts *runFacts, link string, comment *commentOut) error

// appRef is the publishing slice of a registered app (APPS-01/02).
type appRef struct {
	ID            uuid.UUID
	Owner         string
	Repo          string
	CommentMarker string
}

// runFacts is one run's publish snapshot: identity, GitHub pointers and
// the detail view (per-test attachments, duration) the comment renders.
type runFacts struct {
	App        appRef
	RunID      uuid.UUID
	HeadSHA    string
	DurationMS int64
	CheckRunID pgtype.Int8
	Rep        verdict.Report
	Detail     *runs.RunDetail
}

// commentOut is the sticky-comment write result: nil when the run had no
// open PR (nothing published, nothing persisted).
type commentOut struct {
	URL string
	PR  int
	ID  int64
}

// NewPublisher mounts the T10 publisher on a client + origins from env.
func NewPublisher(gh *github.Client, reportBase, linkSecret string, logger *slog.Logger) *Publisher {
	return &Publisher{
		gh:         gh,
		reportBase: reportBase,
		linkSecret: linkSecret,
		logger:     logger,
		load:       loadFacts,
		record:     recordURLs,
	}
}

// Publish implements ReportPublisher.
//
// Sub-steps are independent; only non-transient local errors (fact load,
// URL persistence) propagate as job errors so River retries. GitHub-side
// rate-limit exhaustion skips the remaining writes silently — observational
// budget, never a retry loop.
func (p *Publisher) Publish(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID, rep verdict.Report) error {
	if rem := p.gh.Remaining(); rem >= 0 && rem <= budgetFloor {
		p.logger.Warn("report: rate budget at floor; skipping comment/check-run update",
			"run_id", runID, "remaining", rem)
		return nil
	}

	facts, err := p.load(ctx, pool, runID, rep)
	if err != nil {
		return fmt.Errorf("report: load publish facts: %w", err)
	}

	// REPT-01: the signed report link verifies without shell auth.
	link := ReportLink(runID, p.reportBase, p.linkSecret)

	comment, err := p.publishComment(ctx, facts, link)
	if err != nil {
		if !rateLimited(err) {
			return err
		}
		comment = nil
	}
	if err := p.publishCheckRun(ctx, facts, link); err != nil && !rateLimited(err) {
		return err
	}
	return p.record(ctx, pool, runID, facts, link, comment)
}

// rateLimited reports err as the client's fail-fast budget exhaustion.
func rateLimited(err error) bool {
	var rle *github.RateLimitError
	return errors.As(err, &rle)
}

// publishComment resolves the open PR for the run's head SHA and creates
// or PATCHes the sticky comment. No PR (workflow_dispatch deploys) → skip.
func (p *Publisher) publishComment(ctx context.Context, facts *runFacts, link string) (*commentOut, error) {
	pulls, err := p.gh.ListPullsForCommit(ctx, facts.App.Owner, facts.App.Repo, facts.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("resolve pr by head sha: %w", err)
	}
	pr := 0
	for _, pull := range pulls {
		if pull.State == "open" {
			pr = pull.Number
			break
		}
	}
	if pr == 0 {
		p.logger.Info("report: no open PR for head sha; comment skipped", "head_sha", facts.HeadSHA)
		return nil, nil
	}

	body := renderComment(facts.App.CommentMarker, facts.Rep, topFailures(facts), link, p.reportBase,
		facts.DurationMS, facts.RunID, reportToken(facts.RunID, p.linkSecret))
	comments, err := p.gh.ListIssueComments(ctx, facts.App.Owner, facts.App.Repo, pr)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	var existing *github.IssueComment
	for i := range comments {
		if strings.Contains(comments[i].Body, facts.App.CommentMarker) {
			existing = &comments[i]
			break
		}
	}
	if existing == nil {
		created, err := p.gh.CreateIssueComment(ctx, facts.App.Owner, facts.App.Repo, pr, body)
		if err != nil {
			return nil, fmt.Errorf("create comment: %w", err)
		}
		return &commentOut{URL: created.HTMLURL, PR: pr, ID: created.ID}, nil
	}
	updated, err := p.gh.UpdateIssueComment(ctx, facts.App.Owner, facts.App.Repo, existing.ID, body)
	if err != nil {
		return nil, fmt.Errorf("update comment %d: %w", existing.ID, err)
	}
	return &commentOut{URL: updated.HTMLURL, PR: pr, ID: updated.ID}, nil
}

// loadFacts reads the run detail + app identity from Postgres.
func loadFacts(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID, rep verdict.Report) (*runFacts, error) {
	detail, err := runs.GetRunDetail(ctx, pool, toPgUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("run detail: %w", err)
	}
	if detail == nil || detail.Run == nil {
		return nil, fmt.Errorf("run %s not found", runID)
	}
	var app appRef
	app.ID = uuid.UUID(detail.Run.AppID.Bytes)
	if err := pool.QueryRow(ctx, `
		select owner, repo, comment_marker
		from registered_apps
		where id = $1
	`, detail.Run.AppID).Scan(&app.Owner, &app.Repo, &app.CommentMarker); err != nil {
		return nil, fmt.Errorf("load app %s: %w", app.ID, err)
	}
	return &runFacts{
		App:        app,
		RunID:      runID,
		HeadSHA:    detail.Run.HeadSHA,
		DurationMS: int64(detail.Run.DurationMS.Int32),
		CheckRunID: detail.Run.CheckRunID,
		Rep:        rep,
		Detail:     detail,
	}, nil
}

// recordURLs persists the publish outputs in one tx: the report link +
// comment URL on the run row and the pr_comments upsert (REPT-03 state).
func recordURLs(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID, facts *runFacts, link string, comment *commentOut) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin record urls: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `update runs set report_url = $2 where id = $1`, runID, link); err != nil {
		return fmt.Errorf("persist report_url: %w", err)
	}
	if comment != nil {
		if _, err := tx.Exec(ctx, `update runs set comment_url = $2 where id = $1`, runID, comment.URL); err != nil {
			return fmt.Errorf("persist comment_url: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into pr_comments (app_id, pr_number, comment_id, last_run_id)
			values ($1, $2, $3, $4)
			on conflict (app_id, pr_number) do update
			set comment_id = excluded.comment_id,
			    last_run_id = excluded.last_run_id,
			    updated_at = now()
		`, facts.App.ID, comment.PR, comment.ID, runID); err != nil {
			return fmt.Errorf("upsert pr_comments: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit record urls: %w", err)
	}
	return nil
}

// --- comment rendering ---

// failureRow is one rendered "top failures" entry.
type failureRow struct {
	Title  string
	File   string
	Class  verdict.Class
	Reason string
	Shots  []string // path-sanitized screenshot attachments
}

// topFailures selects the first maxFailures FAIL tests with their
// path-sanitized screenshot attachments (from the run's test_results rows,
// matched by title). Deterministic: rep.Tests order (suite/file order).
func topFailures(facts *runFacts) []failureRow {
	shots := map[string][]string{}
	if facts.Detail != nil {
		for _, r := range facts.Detail.TestResults {
			shots[r.Title] = shotPaths(r.Attachments)
		}
	}
	var out []failureRow
	for _, tv := range facts.Rep.Tests {
		if tv.Verdict != verdict.VerdictFail {
			continue
		}
		out = append(out, failureRow{
			Title:  tv.Title,
			File:   tv.File,
			Class:  tv.Class,
			Reason: tv.Reason,
			Shots:  shots[tv.Title],
		})
		if len(out) == maxFailures {
			break
		}
	}
	return out
}

// shotPaths decodes a test_results attachments jsonb array ([{kind,path}])
// to the sanitized screenshot paths.
func shotPaths(att []byte) []string {
	var refs []struct {
		Kind string `json:"kind"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(att, &refs); err != nil {
		return nil
	}
	var out []string
	for _, r := range refs {
		if r.Kind == "screenshot" {
			if safe, ok := sanitizeArtifactPath(r.Path); ok {
				out = append(out, safe)
			}
		}
	}
	return out
}

// sanitizeArtifactPath accepts only plain volume-relative artifact paths
// (SECR-01 traversal safety): no absolute, no "..", no backslashes, and
// every segment restricted to the same charset Playwright writes. The
// second return is false when the path is unsafe to embed in a URL.
func sanitizeArtifactPath(p string) (string, bool) {
	if p == "" || strings.Contains(p, "..") {
		return "", false
	}
	p = path.Clean(p)
	if strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsRune(p, '\\') {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		for _, r := range seg {
			ok := r == '-' || r == '.' || r == '_' || r >= '0' && r <= '9' ||
				r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
			if !ok {
				return "", false
			}
		}
	}
	return p, true
}

// renderComment builds the sticky body: marker (create/update lookup), a
// verdict header with counts + duration, the signed report link and the
// top failures. All evidence-derived text is HTML-escaped — test titles,
// error messages and note are attacker-controllable data (no injection).
func renderComment(marker string, rep verdict.Report, failures []failureRow, link, artifactBase string,
	durationMS int64, runID uuid.UUID, token string) string {
	var b bytes.Buffer
	b.WriteString(marker + "\n\n")
	fmt.Fprintf(&b, "## e2ai run — %s\n\n", rep.Verdict)
	if rep.Note != "" {
		fmt.Fprintf(&b, "> %s\n\n", html.EscapeString(rep.Note))
	}
	fmt.Fprintf(&b, "%d/%d passed · %d failed · %d flaky · %d skipped · %s\n\n",
		rep.Counts.Pass, rep.Counts.Total(), rep.Counts.Fail, rep.Counts.Flaky,
		rep.Counts.Inconclusive+rep.Counts.InfraError, durationStr(durationMS))
	b.WriteString(fmt.Sprintf("Full report: %s\n\n", link))
	if len(failures) > 0 {
		b.WriteString("### Top failures\n\n")
		for _, f := range failures {
			fmt.Fprintf(&b, "- **%s** `%s` (%s)\n",
				html.EscapeString(f.Title), html.EscapeString(f.File), html.EscapeString(string(f.Class)))
			if f.Reason != "" {
				fmt.Fprintf(&b, "  %s\n", html.EscapeString(f.Reason))
			}
			for _, shot := range f.Shots {
				// Screenshots are token-gated: the URL carries the run id
				// (volume layout is <root>/<run id>/…) and the same ?e2ai=
				// HMAC token the report link uses, so images render outside
				// shell auth.
				fmt.Fprintf(&b, "  <img src=\"%s/e2ai/artifacts/%s/%s?e2ai=%s\" width=\"480\" alt=\"failure screenshot\">\n",
					strings.TrimRight(artifactBase, "/"), runID, shot, token)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// durationStr renders a millisecond duration for the comment (1m42s).
func durationStr(ms int64) string {
	if ms <= 0 {
		return "0s"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	sec := ms / 1000
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm%ds", sec/60, sec%60)
}

// --- signed report link (REPT-01) ---

// ReportLink builds the run's report-page URL:
//
//	{reportBase}/e2ai/runs/{runID}?e2ai={hex(hmac-sha256(runID, secret))}
//
// An empty secret (local dev) yields the unsigned URL.
func ReportLink(runID uuid.UUID, reportBase, secret string) string {
	base := strings.TrimRight(reportBase, "/") + "/e2ai/runs/" + runID.String()
	if secret == "" {
		return base
	}
	return base + "?e2ai=" + reportToken(runID, secret)
}

// VerifyReportToken checks a report-page ?e2ai= token in constant time.
// An empty secret is dev mode: only unsigned links verify.
func VerifyReportToken(runID uuid.UUID, token, secret string) bool {
	if secret == "" {
		return token == ""
	}
	return hmac.Equal([]byte(reportToken(runID, secret)), []byte(token))
}

func reportToken(runID uuid.UUID, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(runID.String()))
	return hex.EncodeToString(mac.Sum(nil))
}
