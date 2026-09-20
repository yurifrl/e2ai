// diff.go — DiffSource implementors over the github client and the
// PR-known-vs-compare preference rule (DERV-01).
package derive

import (
	"context"
	"fmt"

	"github.com/NSXBet/e2ai/api/internal/platform/github"
)

// githubClient is the slice of *github.Client this package needs.
type githubClient interface {
	ListPullsForCommit(ctx context.Context, owner, repo, sha string) ([]github.CommitPull, error)
	Compare(ctx context.Context, owner, repo, base, head string) (github.DiffResult, error)
	ListPRFiles(ctx context.Context, owner, repo string, number int) ([]github.CompareFile, error)
}

// Diff is the production DiffSource. The PR path wins when the caller has a
// PR number (per-PR files endpoint: no 300-file/250-commit compare cap, so
// no truncation flag), the three-dot compare fallback runs otherwise and
// forwards its truncation flag so the degradation stays visible in
// derivation output.
type Diff struct {
	Client githubClient
	Owner  string
	Repo   string
}

// ChangedPaths implements DiffSource via the three-dot compare.
func (d Diff) ChangedPaths(ctx context.Context, base, head string) ([]string, bool, error) {
	if d.Client == nil {
		return nil, false, fmt.Errorf("derive: nil github client")
	}
	res, err := d.Client.Compare(ctx, d.Owner, d.Repo, base, head)
	if err != nil {
		return nil, false, err
	}
	paths := make([]string, 0, len(res.Files))
	for _, f := range res.Files {
		paths = append(paths, f.Filename)
	}
	return paths, res.DiffTruncated, nil
}

// ChangedPathsForPR is the PR-known branch: the webhook names the PR, so no
// compare round-trip is needed at all and the truncation flag is always
// false.
func (d Diff) ChangedPathsForPR(ctx context.Context, number int) ([]string, error) {
	if d.Client == nil {
		return nil, fmt.Errorf("derive: nil github client")
	}
	files, err := d.Client.ListPRFiles(ctx, d.Owner, d.Repo, number)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Filename)
	}
	return paths, nil
}

// ChangedPathsResolved is the preference rule (DERV-01) in one call for the
// derive job: resolve the PR for a deploy context, then take per-PR files
// when a PR is known — no 300-file/250-commit compare cap — and fall back
// to the three-dot merge-base compare otherwise, surfacing the compare
// response's truncation flag so scope can record degraded coverage.
func (d Diff) ChangedPathsResolved(ctx context.Context, base, head, headSHA string, webhookPRs []int) (paths []string, truncated bool, prNumber int, err error) {
	prNumber, err = d.resolvePR(ctx, webhookPRs, headSHA)
	if err != nil {
		return nil, false, 0, err
	}
	if prNumber != 0 {
		paths, err = d.ChangedPathsForPR(ctx, prNumber)
		return paths, false, prNumber, err
	}
	paths, truncated, err = d.ChangedPaths(ctx, base, head)
	return paths, truncated, 0, err
}

// resolvePR picks the PR number for a deploy context. The webhook's
// pull_requests list wins when populated; it is empty for non-push
// triggers (e.g. workflow_dispatch), where PRs resolve from the head SHA
// via the commits/{sha}/pulls endpoint instead. Zero means "no PR known" —
// callers fall back to the compare.
func (d Diff) resolvePR(ctx context.Context, webhookPRs []int, headSHA string) (int, error) {
	if len(webhookPRs) > 0 {
		return webhookPRs[0], nil
	}
	if headSHA == "" {
		return 0, nil
	}
	if d.Client == nil {
		return 0, fmt.Errorf("derive: nil github client")
	}
	pulls, err := d.Client.ListPullsForCommit(ctx, d.Owner, d.Repo, headSHA)
	if err != nil {
		return 0, err
	}
	for _, p := range pulls {
		if p.State == "open" {
			return p.Number, nil
		}
	}
	if len(pulls) > 0 {
		return pulls[0].Number, nil
	}
	return 0, nil
}
