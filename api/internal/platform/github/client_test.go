package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recorder captures the last request per registered handler.
type recorder struct {
	method      string
	path        string
	query       string
	auth        string
	contentType string
	accept      string
	apiVersion  string
	body        string
}

func newFake(t *testing.T, status int, response string) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.auth = r.Header.Get("Authorization")
		rec.accept = r.Header.Get("Accept")
		rec.apiVersion = r.Header.Get("X-GitHub-Api-Version")
		rec.contentType = r.Header.Get("Content-Type")
		if r.Body != nil {
			var sb strings.Builder
			dec := json.NewDecoder(r.Body)
			var raw any
			if err := dec.Decode(&raw); err == nil {
				b, _ := json.Marshal(raw)
				sb.Write(b)
			}
			rec.body = sb.String()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	client, err := NewWithBaseURL("test-pat", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return client, rec
}

func TestCreateThenUpdateIssueComment(t *testing.T) {
	ctx := context.Background()
	var created int
	client, rec := newFake(t, http.StatusCreated, `{"id":42,"body":"<!-- e2ai:run:bet-app-web --> v1","html_url":"https://github.com/NSXBet/bet-app/pull/7#issuecomment-42","created_at":"2026-09-20T00:00:00Z","updated_at":"2026-09-20T00:00:00Z"}`)

	c1, err := client.CreateIssueComment(ctx, "NSXBet", "bet-app", 7, "<!-- e2ai:run:bet-app-web --> v1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c1.ID != 42 {
		t.Errorf("create ID = %d, want 42", c1.ID)
	}
	created++
	if rec.method != http.MethodPost || rec.path != "/repos/NSXBet/bet-app/issues/7/comments" {
		t.Errorf("create call = %s %s", rec.method, rec.path)
	}
	if rec.auth != "Bearer test-pat" || rec.accept != acceptType || rec.apiVersion != apiVersion {
		t.Errorf("headers = auth %q accept %q version %q", rec.auth, rec.accept, rec.apiVersion)
	}
	if !strings.Contains(rec.body, "e2ai:run:bet-app-web") {
		t.Errorf("create body missing marker: %s", rec.body)
	}

	c2, err := client.UpdateIssueComment(ctx, "NSXBet", "bet-app", 42, "<!-- e2ai:run:bet-app-web --> v2")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if c2.ID != 42 {
		t.Errorf("update ID = %d, want 42", c2.ID)
	}
	if rec.method != http.MethodPatch || rec.path != "/repos/NSXBet/bet-app/issues/comments/42" {
		t.Errorf("update call = %s %s", rec.method, rec.path)
	}
	if !strings.Contains(rec.body, "v2") {
		t.Errorf("update body = %s, want v2", rec.body)
	}
	_ = created
}

func TestListPullsForCommit(t *testing.T) {
	client, rec := newFake(t, http.StatusOK, `[{"number":7,"state":"open","title":"Deploy web","html_url":"https://github.com/NSXBet/bet-app/pull/7","draft":false}]`)

	pulls, err := client.ListPullsForCommit(context.Background(), "NSXBet", "bet-app", "abc123")
	if err != nil {
		t.Fatalf("list pulls: %v", err)
	}
	if len(pulls) != 1 || pulls[0].Number != 7 || pulls[0].State != "open" || pulls[0].HTMLURL == "" {
		t.Errorf("pulls = %+v", pulls)
	}
	if rec.path != "/repos/NSXBet/bet-app/commits/abc123/pulls" {
		t.Errorf("path = %s", rec.path)
	}
	if !strings.Contains(rec.query, "per_page=100") {
		t.Errorf("query = %s, want per_page=100", rec.query)
	}

}

func TestListPullsForCommitEmpty(t *testing.T) {
	client, _ := newFake(t, http.StatusOK, `[]`)
	pulls, err := client.ListPullsForCommit(context.Background(), "NSXBet", "bet-app", "deadbeef")
	if err != nil {
		t.Fatalf("empty list: %v", err)
	}
	if len(pulls) != 0 {
		t.Errorf("pulls = %+v, want empty", pulls)
	}
}

func TestCheckRunCreateAndPatchBodyShape(t *testing.T) {
	ctx := context.Background()
	client, rec := newFake(t, http.StatusCreated, `{"id":9,"status":"in_progress","name":"e2ai:bet-app-web","head_sha":"abc123"}`)

	created, err := client.CreateCheckRun(ctx, "NSXBet", "bet-app", CreateCheckRunInput{
		Name:    "e2ai:bet-app-web",
		HeadSHA: "abc123",
		Status:  "in_progress",
	})
	if err != nil {
		t.Fatalf("create check run: %v", err)
	}
	if created.ID != 9 || created.Status != "in_progress" {
		t.Errorf("created = %+v", created)
	}
	if rec.path != "/repos/NSXBet/bet-app/check-runs" || rec.method != http.MethodPost {
		t.Errorf("create call = %s %s", rec.method, rec.path)
	}

	var createBody struct {
		Name       string          `json:"name"`
		HeadSHA    string          `json:"head_sha"`
		Status     string          `json:"status"`
		Conclusion json.RawMessage `json:"conclusion"`
	}
	if err := json.Unmarshal([]byte(rec.body), &createBody); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	if createBody.HeadSHA != "abc123" || createBody.Status != "in_progress" || string(createBody.Conclusion) != "" {
		t.Errorf("create body shape = %+v", createBody)
	}

	// Patch: report contract pins status=completed, conclusion=neutral.
	client2, rec2 := newFake(t, http.StatusOK, `{}`)
	err = client2.UpdateCheckRun(ctx, "NSXBet", "bet-app", 9, UpdateCheckRunInput{
		Status:     "completed",
		Conclusion: "neutral",
		Output:     &CheckRunOutput{Title: "QA report", Summary: "4 tests", DetailsURL: "https://e2ai/report/run-1"},
	})
	if err != nil {
		t.Fatalf("patch check run: %v", err)
	}
	if rec2.method != http.MethodPatch || rec2.path != "/repos/NSXBet/bet-app/check-runs/9" {
		t.Errorf("patch call = %s %s", rec2.method, rec2.path)
	}
	var patchBody struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		Output     struct {
			Title      string `json:"title"`
			Summary    string `json:"summary"`
			DetailsURL string `json:"details_url"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(rec2.body), &patchBody); err != nil {
		t.Fatalf("decode patch body: %v", err)
	}
	if patchBody.Status != "completed" {
		t.Errorf("patch status = %q, want completed", patchBody.Status)
	}
	if patchBody.Conclusion != "neutral" {
		t.Errorf("patch conclusion = %q, want neutral (report-only contract)", patchBody.Conclusion)
	}
	if patchBody.Output.Title != "QA report" || patchBody.Output.Summary != "4 tests" || patchBody.Output.DetailsURL != "https://e2ai/report/run-1" {
		t.Errorf("patch output = %+v", patchBody.Output)
	}
}

func TestListIssueCommentsAndListCompletedRuns(t *testing.T) {
	ctx := context.Background()
	client, rec := newFake(t, http.StatusOK, `[{"id":1,"body":"<!-- e2ai:run:bet-app-web --> stale","html_url":"https://github.com/NSXBet/bet-app/pull/7#issuecomment-1"}]`)

	comments, err := client.ListIssueComments(ctx, "NSXBet", "bet-app", 7)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if len(comments) != 1 || comments[0].ID != 1 || !strings.Contains(comments[0].Body, "e2ai:run") {
		t.Errorf("comments = %+v", comments)
	}
	if !strings.Contains(rec.query, "per_page=100") {
		t.Errorf("query = %s", rec.query)
	}

	client2, rec2 := newFake(t, http.StatusOK, `{"total_count":2,"workflow_runs":[{"id":101,"run_number":5,"event":"workflow_dispatch","status":"completed","conclusion":"success","head_sha":"sha5","head_branch":"main","display_title":"Deploy web","html_url":"https://github.com/NSXBet/bet-app/actions/runs/101","created_at":"2026-09-19T10:00:00Z"},{"id":102,"run_number":6,"event":"workflow_dispatch","status":"in_progress","conclusion":"","head_sha":"sha6","head_branch":"main","display_title":"Deploy web","html_url":"","created_at":"2026-09-19T11:00:00Z"}]}`)
	since := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	runs, err := client2.ListCompletedRuns(ctx, "NSXBet", "bet-app", "deploy.yml", since)
	if err != nil {
		t.Fatalf("list completed runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want only the completed one", runs)
	}
	if runs[0].ID != 101 || runs[0].RunNumber != 5 || runs[0].HeadSHA != "sha5" || runs[0].DisplayTitle != "Deploy web" || runs[0].Conclusion != "success" {
		t.Errorf("run = %+v", runs[0])
	}
	if !strings.Contains(rec2.query, "workflow_id=deploy.yml") || !strings.Contains(rec2.query, "event=workflow_dispatch") {
		t.Errorf("query = %s", rec2.query)
	}
	if !strings.Contains(rec2.query, "created=%3E2026-09-19T09%3A00%3A00Z") {
		t.Errorf("query = %s, want encoded >since", rec2.query)
	}
}

func TestRateLimitExhaustedFailsFast(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	client, err := NewWithBaseURL("test-pat", srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := client.ListPullsForCommit(ctx, "o", "r", "sha"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if client.Remaining() != 0 {
		t.Errorf("remaining = %d, want 0 after header", client.Remaining())
	}
	_, err = client.ListPullsForCommit(ctx, "o", "r", "sha")
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("second call err = %v, want *RateLimitError", err)
	}
	if rle.Reset.Before(time.Now()) {
		t.Errorf("reset = %v, want future", rle.Reset)
	}
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (fail-fast, no retry loop)", hits)
	}
}

func TestAPIErrorParsing(t *testing.T) {
	client, _ := newFake(t, http.StatusNotFound, `{"message":"Not Found","documentation_url":"https://docs.github.com"}`)
	_, err := client.ListPullsForCommit(context.Background(), "o", "r", "sha")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound || apiErr.Message != "Not Found" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestNewFromEnvAndEmptyToken(t *testing.T) {
	if _, err := NewFromEnv(func(string) string { return "" }); err == nil {
		t.Error("empty GITHUB_TOKEN must be rejected")
	}
	c, err := NewFromEnv(func(k string) string { return "tok" })
	if err != nil || c == nil {
		t.Fatalf("NewFromEnv = %v, %v", c, err)
	}
}
