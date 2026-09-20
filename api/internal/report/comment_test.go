// Tests for the T10 publisher: sticky-comment create-then-update, the
// always-neutral check-run conclusion, HTML-safe comment rendering, the
// signed report link, and the rate-limit skip paths — all against a
// stateful fake GitHub (no database needed; facts are mounted in memory).
package report

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NSXBet/e2ai/api/internal/platform/github"
	"github.com/NSXBet/e2ai/api/internal/verdict"
)

// fakeGH is a stateful GitHub fake: it stores PRs by head sha, issue
// comments per PR and the last check-run patch body, and counts requests.
type fakeGH struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	pulls    map[string][]github.CommitPull // sha -> open PRs
	comments map[int][]github.IssueComment  // pr -> comments
	patches  map[int64]checkRunPatch        // check-run id -> last patch
	nextID   int64                          // next comment id
	requests int                            // total HTTP requests served
	rate     string                         // X-RateLimit-Remaining to emit ("" = none)
}

// checkRunPatch mirrors UpdateCheckRunInput for body assertions.
type checkRunPatch struct {
	Status     string          `json:"status"`
	Conclusion string          `json:"conclusion"`
	Output     *checkRunOutput `json:"output"`
}

type checkRunOutput struct {
	Title      string `json:"title"`
	Summary    string `json:"summary"`
	DetailsURL string `json:"details_url"`
}

type commentBody struct {
	Body string `json:"body"`
}

func newFakeGH(t *testing.T) *fakeGH {
	t.Helper()
	f := &fakeGH{
		t:        t,
		pulls:    map[string][]github.CommitPull{},
		comments: map[int][]github.IssueComment{},
		patches:  map[int64]checkRunPatch{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/NSXBet/bet-app/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		f.emitRate(w)
		pulls := f.pulls[r.PathValue("sha")]
		writeJSON(t, w, pulls)
	})
	mux.HandleFunc("GET /repos/NSXBet/bet-app/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		f.emitRate(w)
		var n int
		fmt.Sscanf(r.PathValue("n"), "%d", &n)
		writeJSON(t, w, f.comments[n])
	})
	mux.HandleFunc("POST /repos/NSXBet/bet-app/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		f.emitRate(w)
		var cb commentBody
		readJSON(t, r, &cb)
		var n int
		fmt.Sscanf(r.PathValue("n"), "%d", &n)
		f.mu.Lock()
		f.nextID++
		id := f.nextID
		c := github.IssueComment{ID: id, Body: cb.Body, HTMLURL: fmt.Sprintf("https://github.com/NSXBet/bet-app/issues/%d#issuecomment-%d", n, id)}
		f.comments[n] = append(f.comments[n], c)
		f.mu.Unlock()
		writeJSON(t, w, c)
	})
	mux.HandleFunc("PATCH /repos/NSXBet/bet-app/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		f.emitRate(w)
		var cb commentBody
		readJSON(t, r, &cb)
		var id int64
		fmt.Sscanf(r.PathValue("id"), "%d", &id)
		f.mu.Lock()
		for pr, list := range f.comments {
			for i := range list {
				if list[i].ID == id {
					f.comments[pr][i].Body = cb.Body
				}
			}
		}
		f.mu.Unlock()
		writeJSON(t, w, github.IssueComment{ID: id, Body: cb.Body, HTMLURL: fmt.Sprintf("https://github.com/NSXBet/bet-app/issues/7#issuecomment-%d", id)})
	})
	mux.HandleFunc("PATCH /repos/NSXBet/bet-app/check-runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		f.emitRate(w)
		var p checkRunPatch
		readJSON(t, r, &p)
		var id int64
		fmt.Sscanf(r.PathValue("id"), "%d", &id)
		f.mu.Lock()
		f.patches[id] = p
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGH) note(r *http.Request) {
	f.mu.Lock()
	f.requests++
	f.mu.Unlock()
}

// emitRate sets the rate-limit header the publisher's observational budget
// reads. Empty string emits nothing (client remaining stays at -1 or its
// last observed value).
func (f *fakeGH) emitRate(w http.ResponseWriter) {
	if f.rate != "" {
		w.Header().Set("X-RateLimit-Remaining", f.rate)
	}
}

func (f *fakeGH) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeGH) commentCount(pr int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.comments[pr])
}

func (f *fakeGH) commentBodyAt(pr, i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.comments[pr][i].Body
}

func (f *fakeGH) commentIDAt(pr, i int) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.comments[pr][i].ID
}

func (f *fakeGH) lastPatch(id int64) checkRunPatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.patches[id]
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("fake write: %v", err)
	}
}

func readJSON(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Errorf("fake read: %v", err)
	}
}

// --- fixtures ---

var (
	testReportBase = "https://reports.example.com"
	testSecret     = "e2ai-link-secret"
	testMarker     = "<!-- e2ai-report -->"
)

// testFacts builds a passing-run publish snapshot with an open PR.
func testFacts(rep verdict.Report) *runFacts {
	return &runFacts{
		App:        appRef{ID: uuid.Must(uuid.NewRandom()), Owner: "NSXBet", Repo: "bet-app", CommentMarker: testMarker},
		RunID:      uuid.Must(uuid.NewRandom()),
		HeadSHA:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DurationMS: 42_000,
		CheckRunID: pgtype.Int8{Int64: 9001, Valid: true},
		Rep:        rep,
	}
}

// testPublisher mounts a Publisher whose facts/recorder are in-memory.
func testPublisher(t *testing.T, f *fakeGH, facts *runFacts) (*Publisher, *[]recordedRun) {
	t.Helper()
	gh, err := github.NewWithBaseURL("test-token", f.srv.URL)
	if err != nil {
		t.Fatalf("fake client: %v", err)
	}
	p := &Publisher{
		gh:         gh,
		reportBase: testReportBase,
		linkSecret: testSecret,
		logger:     slog.Default(),
	}
	var recorded []recordedRun
	p.load = func(context.Context, *pgxpool.Pool, uuid.UUID, verdict.Report) (*runFacts, error) {
		return facts, nil
	}
	p.record = recordURLsRecorder(&recorded)
	return p, &recorded
}

// recordedRun is one record() invocation.
type recordedRun struct {
	RunID   uuid.UUID
	Link    string
	Comment *commentOut
}

func recordURLsRecorder(out *[]recordedRun) recordFunc {
	return func(_ context.Context, _ *pgxpool.Pool, id uuid.UUID, _ *runFacts, link string, comment *commentOut) error {
		*out = append(*out, recordedRun{RunID: id, Link: link, Comment: comment})
		return nil
	}
}

// --- tests ---

func TestPublishNoOpenPRSkipsCommentFinalizesCheckRun(t *testing.T) {
	f := newFakeGH(t) // no pulls registered
	facts := testFacts(verdict.Report{Verdict: verdict.VerdictPass, Counts: verdict.Counts{Pass: 3}})
	p, recorded := testPublisher(t, f, facts)

	if err := p.Publish(context.Background(), nil, uuid.Must(uuid.NewRandom()), verdict.Report{}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if f.commentCount(7) != 0 {
		t.Errorf("comment created without an open PR")
	}
	// The check run is keyed to the head SHA, not the PR: it is finalized
	// even when no PR is open.
	patch, ok := f.patches[9001]
	if !ok || patch.Conclusion != "neutral" || patch.Status != "completed" {
		t.Errorf("check run not finalized neutral: %+v", patch)
	}
	if len(*recorded) != 1 || (*recorded)[0].Comment != nil {
		t.Errorf("want one record with nil comment, got %+v", *recorded)
	}
}

func TestPublishCreatesThenUpdatesStickyComment(t *testing.T) {
	f := newFakeGH(t)
	f.pulls["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"] =
		[]github.CommitPull{{Number: 7, State: "open", Title: "Deploy web"}}

	passed := verdict.Report{Verdict: verdict.VerdictPass, Counts: verdict.Counts{Pass: 4}}
	failed := verdict.Report{
		Verdict: verdict.VerdictFail,
		Counts:  verdict.Counts{Pass: 2, Fail: 1},
		Tests: []verdict.TestVerdict{{
			ID: "t1", Title: "edits a bet", File: "tests/bets.spec.ts",
			Verdict: verdict.VerdictFail, Class: verdict.ClassTestGap,
			Reason: `locator "button:has-text(\"Save\")" not found`,
		}},
	}

	runID := uuid.Must(uuid.NewRandom())
	facts := testFacts(passed)
	p, recorded := testPublisher(t, f, facts)

	// First run: create.
	if err := p.Publish(context.Background(), nil, runID, passed); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if n := f.commentCount(7); n != 1 {
		t.Fatalf("after first publish: %d comments, want 1", n)
	}
	firstBody := f.commentBodyAt(7, 0)
	if !strings.Contains(firstBody, testMarker) {
		t.Errorf("body missing sticky marker: %q", firstBody)
	}

	// Second run: same facts, updated verdict — must PATCH the same comment.
	facts.Rep = failed
	if err := p.Publish(context.Background(), nil, runID, failed); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if n := f.commentCount(7); n != 1 {
		t.Fatalf("after second publish: %d comments, want 1 (update, not create)", n)
	}
	secondBody := f.commentBodyAt(7, 0)
	if !strings.Contains(secondBody, string(verdict.VerdictFail)) {
		t.Errorf("comment not updated to FAIL: %q", secondBody)
	}
	if strings.Contains(secondBody, "e2ai run — PASS") {
		t.Errorf("stale PASS header still present: %q", secondBody)
	}
	if len(*recorded) != 2 {
		t.Fatalf("recorded %d publishes, want 2", len(*recorded))
	}
	if (*recorded)[0].Comment.ID != (*recorded)[1].Comment.ID {
		t.Errorf("create id %d != update id %d — sticky comment split",
			(*recorded)[0].Comment.ID, (*recorded)[1].Comment.ID)
	}
}

func TestPublishCheckRunConclusionAlwaysNeutral(t *testing.T) {
	verdicts := []verdict.Report{
		{Verdict: verdict.VerdictPass, Counts: verdict.Counts{Pass: 5}},
		{Verdict: verdict.VerdictFail, Counts: verdict.Counts{Fail: 1}},
		{Verdict: verdict.VerdictFlaky, Counts: verdict.Counts{Pass: 2, Flaky: 1}},
		{Verdict: verdict.VerdictInfraError, Counts: verdict.Counts{InfraError: 1}, Note: "execute: no parseable results.json"},
		{Verdict: verdict.VerdictInconclusive, Counts: verdict.Counts{Inconclusive: 2}},
	}
	for _, rep := range verdicts {
		t.Run(string(rep.Verdict), func(t *testing.T) {
			f := newFakeGH(t)
			f.pulls["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"] =
				[]github.CommitPull{{Number: 7, State: "open", Title: "Deploy web"}}
			p, _ := testPublisher(t, f, testFacts(rep))

			if err := p.Publish(context.Background(), nil, uuid.Must(uuid.NewRandom()), rep); err != nil {
				t.Fatalf("publish %s: %v", rep.Verdict, err)
			}
			got := f.lastPatch(9001)
			if got.Status != "completed" {
				t.Errorf("status = %q, want completed", got.Status)
			}
			if got.Conclusion != "neutral" {
				t.Errorf("conclusion = %q, want neutral (report-only invariant)", got.Conclusion)
			}
			if got.Output == nil || got.Output.DetailsURL == "" {
				t.Errorf("output missing details_url: %+v", got.Output)
			}
		})
	}
}

func TestPublishEscapesInjectedTitleAndReason(t *testing.T) {
	f := newFakeGH(t)
	f.pulls["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"] =
		[]github.CommitPull{{Number: 7, State: "open", Title: "Deploy web"}}
	rep := verdict.Report{
		Verdict: verdict.VerdictFail,
		Counts:  verdict.Counts{Fail: 1},
		Tests: []verdict.TestVerdict{{
			ID:      "evil",
			Title:   `<script>alert("pwned")</script> login`,
			File:    "tests/evil.spec.ts",
			Verdict: verdict.VerdictFail,
			Reason:  `<img src=x onerror=alert(1)> boom`,
		}},
	}
	p, _ := testPublisher(t, f, testFacts(rep))

	if err := p.Publish(context.Background(), nil, uuid.Must(uuid.NewRandom()), rep); err != nil {
		t.Fatalf("publish: %v", err)
	}
	body := f.commentBodyAt(7, 0)
	for _, raw := range []string{"<script>", "<img"} {
		if strings.Contains(body, raw) {
			t.Errorf("body contains raw %q (HTML injection):\n%s", raw, body)
		}
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("body missing escaped script tag:\n%s", body)
	}
}

func TestPublishSkipsAtRateBudgetFloor(t *testing.T) {
	f := newFakeGH(t)
	f.rate = "3" // at/below the floor
	facts := testFacts(verdict.Report{Verdict: verdict.VerdictPass, Counts: verdict.Counts{Pass: 1}})
	p, recorded := testPublisher(t, f, facts)
	// The budget is observational: remaining is known only after a response.
	// Seed it on the publisher's own client so the budget check sees it.
	if _, err := p.gh.ListPullsForCommit(context.Background(), "NSXBet", "bet-app", "seed"); err != nil {
		t.Fatalf("seed remaining: %v", err)
	}
	if err := p.Publish(context.Background(), nil, uuid.Must(uuid.NewRandom()), verdict.Report{}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := f.hits(); got != 1 {
		t.Errorf("%d requests served after skip, want 1 (the seeding call only)", got)
	}
	if len(*recorded) != 0 {
		t.Errorf("record ran despite skipped publish: %+v", *recorded)
	}
}

func TestPublishToleratesMidFlightRateLimit(t *testing.T) {
	f := newFakeGH(t)
	f.rate = "0" // exhausts the budget after the first call
	f.pulls["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"] =
		[]github.CommitPull{{Number: 7, State: "open", Title: "Deploy web"}}
	facts := testFacts(verdict.Report{Verdict: verdict.VerdictPass, Counts: verdict.Counts{Pass: 1}})
	p, recorded := testPublisher(t, f, facts)

	runID := uuid.Must(uuid.NewRandom())
	if err := p.Publish(context.Background(), nil, runID, verdict.Report{}); err != nil {
		t.Fatalf("publish: mid-flight rate limit must never re-fail the run: %v", err)
	}
	// Only the pulls call went out; comment + check-run were skipped.
	if got := f.hits(); got != 1 {
		t.Errorf("%d requests, want 1", got)
	}
	// The produced link is still recorded (REPT-01) without a comment URL.
	if len(*recorded) != 1 {
		t.Fatalf("recorded %d, want 1", len(*recorded))
	}
	if (*recorded)[0].Comment != nil {
		t.Errorf("comment recorded despite rate limit: %+v", (*recorded)[0].Comment)
	}
	if (*recorded)[0].Link == "" {
		t.Errorf("report link not recorded")
	}
}

func TestReportLinkTokenVerifiesAndRejects(t *testing.T) {
	runID := uuid.Must(uuid.NewRandom())

	link := ReportLink(runID, testReportBase, testSecret)
	if !strings.HasPrefix(link, testReportBase+"/e2ai/runs/"+runID.String()+"?e2ai=") {
		t.Fatalf("link shape: %q", link)
	}
	token := strings.TrimPrefix(link, testReportBase+"/e2ai/runs/"+runID.String()+"?e2ai=")
	if !VerifyReportToken(runID, token, testSecret) {
		t.Errorf("token must verify with the right key")
	}
	if VerifyReportToken(runID, token, "wrong-secret") {
		t.Errorf("token must reject the wrong key")
	}
	other := uuid.Must(uuid.NewRandom())
	if VerifyReportToken(other, token, testSecret) {
		t.Errorf("token must reject a different run id")
	}

	// Dev mode (no secret): unsigned links verify, signed ones do not.
	unsigned := ReportLink(runID, testReportBase, "")
	if strings.Contains(unsigned, "?") {
		t.Errorf("unsigned link got a token: %q", unsigned)
	}
	if !VerifyReportToken(runID, "", "") {
		t.Errorf("empty token must verify in dev mode")
	}
	if VerifyReportToken(runID, "nonempty", "") {
		t.Errorf("non-empty token must reject in dev mode")
	}
}

func TestSanitizeArtifactPathTraversalSafety(t *testing.T) {
	bad := []string{"", "/etc/passwd", "../secret.png", "a/../b.png", "..", `a\b.png`, "a:b.png", "white space.png"}
	for _, p := range bad {
		if _, ok := sanitizeArtifactPath(p); ok {
			t.Errorf("path %q must be rejected", p)
		}
	}
	good := []string{"screenshots/login-failed.png", "report.data/chunk-1_2.js", "x.Y-09z.html"}
	for _, p := range good {
		if got, ok := sanitizeArtifactPath(p); !ok || got != p {
			t.Errorf("path %q: got (%q, %v)", p, got, ok)
		}
	}
}

// TestPublisherDBRoundTrip exercises the default load/record halves against
// the scratch Postgres: seeded run + app → loadFacts → fake GitHub publish
// → recordURLs persists report_url/comment_url and the pr_comments row
// (REPT-03 state). A second publish stays sticky: one comment, the same
// pr_comments row updated. Skipped without E2AI_TEST_DATABASE_URL, like
// the job tests.
func TestPublisherDBRoundTrip(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedRunAt(t, pool, "executing")
	// Execute runs before report: it creates the check run (in_progress)
	// and stamps runs.check_run_id + duration_ms; report finalizes it.
	if _, err := pool.Exec(context.Background(),
		`update runs set check_run_id = 9001, duration_ms = 42000 where id = $1`, runID); err != nil {
		t.Fatalf("stamp run: %v", err)
	}
	seedTest(t, pool, runID, "lists bets", "tests/bets.spec.ts", "passed", "passed", 1, 700)

	f := newFakeGH(t)
	f.pulls["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"] =
		[]github.CommitPull{{Number: 7, State: "open", Title: "Deploy web"}}
	gh, err := github.NewWithBaseURL("test-token", f.srv.URL)
	if err != nil {
		t.Fatalf("fake client: %v", err)
	}
	p := NewPublisher(gh, testReportBase, testSecret, slog.Default())

	passed := verdict.Report{Verdict: verdict.VerdictPass, Counts: verdict.Counts{Pass: 1}}
	if err := p.Publish(context.Background(), pool, runID, passed); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// runs row: signed report link (token verifies) + comment URL.
	var reportURL, commentURL string
	if err := pool.QueryRow(context.Background(),
		`select report_url, comment_url from runs where id = $1`, runID).Scan(&reportURL, &commentURL); err != nil {
		t.Fatalf("read run urls: %v", err)
	}
	base := testReportBase + "/e2ai/runs/" + runID.String()
	if !strings.HasPrefix(reportURL, base+"?e2ai=") {
		t.Errorf("report url %q, want %q + token", reportURL, base)
	} else if !VerifyReportToken(runID, strings.TrimPrefix(reportURL, base+"?e2ai="), testSecret) {
		t.Errorf("stored report link token does not verify: %q", reportURL)
	}
	if !strings.HasPrefix(commentURL, "https://github.com/NSXBet/bet-app/issues/7#issuecomment-") {
		t.Errorf("comment url %q", commentURL)
	}

	// pr_comments: one row keyed (app_id, pr_number) with the run link.
	var appID pgtype.UUID
	if err := pool.QueryRow(context.Background(),
		`select app_id from runs where id = $1`, runID).Scan(&appID); err != nil {
		t.Fatalf("read app id: %v", err)
	}
	var commentID int64
	var lastRunID uuid.UUID
	var prNumber int
	if err := pool.QueryRow(context.Background(), `
		select pr_number, comment_id, last_run_id
		from pr_comments where app_id = $1
	`, appID).Scan(&prNumber, &commentID, &lastRunID); err != nil {
		t.Fatalf("read pr_comments: %v", err)
	}
	if prNumber != 7 || commentID != f.commentIDAt(7, 0) || lastRunID != runID {
		t.Errorf("pr_comments row: pr=%d id=%d last_run=%s", prNumber, commentID, lastRunID)
	}

	// Second publish (a flaky rerun): comment count stays 1 — sticky.
	flaky := verdict.Report{Verdict: verdict.VerdictFlaky, Counts: verdict.Counts{Pass: 1, Flaky: 1}}
	if err := p.Publish(context.Background(), pool, runID, flaky); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if got := f.commentCount(7); got != 1 {
		t.Errorf("second publish produced %d comments, want 1", got)
	}
	if body := f.commentBodyAt(7, 0); !strings.Contains(body, "e2ai run — FLAKY") {
		t.Errorf("sticky body not updated to FLAKY:\n%s", body)
	}
	var lastRun2 uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`select last_run_id from pr_comments where app_id = $1`, appID).Scan(&lastRun2); err != nil {
		t.Fatalf("read pr_comments: %v", err)
	}
	if lastRun2 != runID {
		t.Errorf("last_run_id %s, want %s", lastRun2, runID)
	}
}
