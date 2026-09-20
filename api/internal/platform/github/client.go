// Package github is a thin, typed REST client over the GitHub API surface
// e2ai needs (REPT-03 sticky comments, neutral check-runs, PR resolution by
// head SHA, reconciliation sweeps).
//
// Deliberately no go-github dependency: stdlib net/http + encoding/json only.
// Rate-limit handling is observational, never a retry loop: every response's
// X-RateLimit-Remaining / X-RateLimit-Reset headers are tracked, and once the
// observed core budget is exhausted the client fails fast with a
// *RateLimitError instead of hammering a 403.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	defaultBaseURL  = "https://api.github.com"
	apiVersion      = "2022-11-28"
	acceptType      = "application/vnd.github+json"
	defaultPageSize = 100
)

// Client is safe for concurrent use. The zero value is not usable; construct
// with New or NewWithBaseURL.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
	// Rate-limit state observed from the last response (PAT-wide core
	// budget). remaining stays -1 until the first response.
	remaining atomic.Int32
	resetUnix atomic.Int64
}

// New returns a client against the public GitHub API. token is the PAT
// (classic or fine-grained); an empty token is rejected.
func New(token string) (*Client, error) {
	return NewWithBaseURL(token, defaultBaseURL)
}

// NewFromEnv builds the client from GITHUB_TOKEN, the single env the stack
// reserves for the PAT (the stored value is an op:// ref resolved upstream).
func NewFromEnv(getenv func(string) string) (*Client, error) {
	return New(getenv("GITHUB_TOKEN"))
}

// NewWithBaseURL overrides the API base — the seam tests use to point the
// client at a fake GitHub (httptest server).
func NewWithBaseURL(token, baseURL string) (*Client, error) {
	if token == "" {
		return nil, fmt.Errorf("github: token must not be empty")
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("github: parse base URL: %w", err)
	}
	c := &Client{base: base, token: token, http: &http.Client{Timeout: 30 * time.Second}}
	c.remaining.Store(-1)
	return c, nil
}

// Remaining reports the rate-limit budget observed from the most recent
// response, or -1 if none observed yet.
func (c *Client) Remaining() int { return int(c.remaining.Load()) }

// RateLimitError is returned when a request is refused because the observed
// core budget is exhausted. It carries the reset instant so callers can back
// off — deliberately, there is no client-side retry loop.
type RateLimitError struct {
	Reset    time.Time
	Endpoint string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("github: rate limit exhausted on %s (resets %s)", e.Endpoint, e.Reset.Format(time.RFC3339))
}

// APIError is a non-2xx response. Message is the GitHub JSON error message
// when present.
type APIError struct {
	StatusCode int
	Message    string
	Endpoint   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s -> %d: %s", e.Endpoint, e.StatusCode, e.Message)
}

// do issues one API call and decodes a 2xx JSON body into out (out may be
// nil for calls with no interesting response). endpoint is an API-relative
// path with optional query.
func (c *Client) do(ctx context.Context, method, endpoint string, body, out any) error {
	if c.remaining.Load() == 0 {
		return &RateLimitError{
			Reset:    time.Unix(c.resetUnix.Load(), 0).UTC(),
			Endpoint: endpoint,
		}
	}

	var payload io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("github: marshal request body: %w", err)
		}
		payload = bytes.NewReader(buf)
	}

	rel, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("github: parse endpoint %q: %w", endpoint, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.ResolveReference(rel).String(), payload)
	if err != nil {
		return fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Accept", acceptType)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	c.observeRateLimit(resp)

	if resp.StatusCode >= 400 {
		apiErr := &APIError{StatusCode: resp.StatusCode, Endpoint: endpoint}
		var envelope struct {
			Message string `json:"message"`
		}
		if json.NewDecoder(resp.Body).Decode(&envelope) == nil && envelope.Message != "" {
			apiErr.Message = envelope.Message
		}
		return apiErr
	}

	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("github: decode response: %w", err)
		}
	}
	return nil
}

// observeRateLimit records X-RateLimit-Remaining / -Reset. A missing header
// (fake servers, proxies) leaves prior state untouched.
func (c *Client) observeRateLimit(resp *http.Response) {
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.remaining.Store(int32(n))
		}
	}
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.resetUnix.Store(n)
		}
	}
}

// --- check runs ---

// CheckRun is the subset of the check-run object e2ai consumes.
type CheckRun struct {
	ID          int64  `json:"id"`
	HTMLURL     string `json:"html_url"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	Name        string `json:"name"`
	HeadSHA     string `json:"head_sha"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
}

// CheckRunOutput is the check-run output block (title/summary/details_url).
type CheckRunOutput struct {
	Title      string `json:"title"`
	Summary    string `json:"summary"`
	DetailsURL string `json:"details_url,omitempty"`
}

// CreateCheckRunInput is the create payload. Status is "queued" or
// "in_progress"; Conclusion is omitted while the run is active.
type CreateCheckRunInput struct {
	Name       string          `json:"name"`
	HeadSHA    string          `json:"head_sha"`
	Status     string          `json:"status"`
	Conclusion string          `json:"conclusion,omitempty"`
	Output     *CheckRunOutput `json:"output,omitempty"`
}

// CreateCheckRun creates a check run on a head SHA.
// POST /repos/{owner}/{repo}/check-runs
func (c *Client) CreateCheckRun(ctx context.Context, owner, repo string, in CreateCheckRunInput) (*CheckRun, error) {
	var run CheckRun
	endpoint := fmt.Sprintf("repos/%s/%s/check-runs", owner, repo)
	if err := c.do(ctx, http.MethodPost, endpoint, in, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// UpdateCheckRunInput is the patch payload. The e2ai report contract always
// patches Status "completed" with Conclusion "neutral" plus an Output block.
type UpdateCheckRunInput struct {
	Status     string          `json:"status,omitempty"`
	Conclusion string          `json:"conclusion,omitempty"`
	Output     *CheckRunOutput `json:"output,omitempty"`
}

// UpdateCheckRun patches an existing check run.
// PATCH /repos/{owner}/{repo}/check-runs/{id}
func (c *Client) UpdateCheckRun(ctx context.Context, owner, repo string, id int64, in UpdateCheckRunInput) error {
	endpoint := fmt.Sprintf("repos/%s/%s/check-runs/%d", owner, repo, id)
	return c.do(ctx, http.MethodPatch, endpoint, in, nil)
}

// --- issue (PR) comments ---

// IssueComment is the subset of an issue comment e2ai consumes. Body is the
// raw markdown; CreatedAt/UpdatedAt are ISO-8601 strings as GitHub returns.
type IssueComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ListIssueComments lists a PR's issue comments (first page, per_page=100 —
// the sticky-comment marker scan never needs deep pagination).
// GET /repos/{owner}/{repo}/issues/{number}/comments
func (c *Client) ListIssueComments(ctx context.Context, owner, repo string, number int) ([]IssueComment, error) {
	var comments []IssueComment
	endpoint := fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=%d", owner, repo, number, defaultPageSize)
	if err := c.do(ctx, http.MethodGet, endpoint, nil, &comments); err != nil {
		return nil, err
	}
	return comments, nil
}

// CreateIssueComment posts a new comment on a PR.
// POST /repos/{owner}/{repo}/issues/{number}/comments
func (c *Client) CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (*IssueComment, error) {
	var comment IssueComment
	endpoint := fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, repo, number)
	if err := c.do(ctx, http.MethodPost, endpoint, map[string]string{"body": body}, &comment); err != nil {
		return nil, err
	}
	return &comment, nil
}

// UpdateIssueComment patches an existing comment's body (the sticky-comment
// update path).
// PATCH /repos/{owner}/{repo}/issues/comments/{commentID}
func (c *Client) UpdateIssueComment(ctx context.Context, owner, repo string, commentID int64, body string) (*IssueComment, error) {
	var comment IssueComment
	endpoint := fmt.Sprintf("repos/%s/%s/issues/comments/%d", owner, repo, commentID)
	if err := c.do(ctx, http.MethodPatch, endpoint, map[string]string{"body": body}, &comment); err != nil {
		return nil, err
	}
	return &comment, nil
}

// --- PR resolution by head SHA ---

// CommitPull is one PR associated with a commit. workflow_run.pull_requests
// is empty for workflow_dispatch runs, so PRs resolve from the head SHA
// instead.
type CommitPull struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
}

// ListPullsForCommit lists PRs whose head commit is sha.
// GET /repos/{owner}/{repo}/commits/{sha}/pulls
func (c *Client) ListPullsForCommit(ctx context.Context, owner, repo, sha string) ([]CommitPull, error) {
	var pulls []CommitPull
	endpoint := fmt.Sprintf("repos/%s/%s/commits/%s/pulls?per_page=%d", owner, repo, sha, defaultPageSize)
	if err := c.do(ctx, http.MethodGet, endpoint, nil, &pulls); err != nil {
		return nil, err
	}
	return pulls, nil
}

// --- actions runs (reconciliation sweep) ---

// WorkflowRun is the subset of an Actions run object the sweep consumes.
type WorkflowRun struct {
	ID           int64  `json:"id"`
	RunNumber    int64  `json:"run_number"`
	Name         string `json:"name"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	HeadSHA      string `json:"head_sha"`
	HeadBranch   string `json:"head_branch"`
	DisplayTitle string `json:"display_title"`
	HTMLURL      string `json:"html_url"`
	CreatedAt    string `json:"created_at"`
}

// RemoteRun is the narrow shape the ingest reconciliation sweep consumes
// (T5's RunsLister contract). Field-for-field compatible with
// ingest.RemoteRun so main.go wires *Client directly.
type RemoteRun struct {
	ID           int64
	RunNumber    int
	HeadSHA      string
	HeadBranch   string
	DisplayTitle string
	Event        string
	Conclusion   string
	HTMLURL      string
}

// ListCompletedRuns lists COMPLETED workflow_dispatch Actions runs for one
// workflow created at or after since — the sweep's feed. Runs that completed
// outside the window (created earlier than since) are excluded; conclusion
// filtering (success only) stays with the caller.
// GET /repos/{owner}/{repo}/actions/runs?workflow_id=...&event=workflow_dispatch&created=>since
func (c *Client) ListCompletedRuns(ctx context.Context, owner, repo, workflowName string, since time.Time) ([]RemoteRun, error) {
	q := url.Values{}
	q.Set("workflow_id", workflowName)
	q.Set("event", "workflow_dispatch")
	q.Set("created", ">"+since.UTC().Format(time.RFC3339))
	q.Set("per_page", strconv.Itoa(defaultPageSize))
	endpoint := "repos/" + owner + "/" + repo + "/actions/runs?" + q.Encode()

	var envelope struct {
		TotalCount   int           `json:"total_count"`
		WorkflowRuns []WorkflowRun `json:"workflow_runs"`
	}
	if err := c.do(ctx, http.MethodGet, endpoint, nil, &envelope); err != nil {
		return nil, err
	}
	runs := make([]RemoteRun, 0, len(envelope.WorkflowRuns))
	for _, r := range envelope.WorkflowRuns {
		if r.Status != "completed" {
			continue
		}
		runs = append(runs, RemoteRun{
			ID:           r.ID,
			RunNumber:    int(r.RunNumber),
			HeadSHA:      r.HeadSHA,
			HeadBranch:   r.HeadBranch,
			DisplayTitle: r.DisplayTitle,
			Event:        r.Event,
			Conclusion:   r.Conclusion,
			HTMLURL:      r.HTMLURL,
		})
	}
	return runs, nil
}
