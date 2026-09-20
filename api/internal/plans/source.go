// Plan sourcing (PLAN-02, research §Pattern 3): a registered app declares
// where its declarative plans live — a path inside the app's own repo
// (`repo_path`) or a dedicated central repo (`central_repo`). Sourcing walks
// the declared location through the GitHub contents API at a ref (HEAD for
// sync; a pinned SHA is equally valid) and returns every markdown plan file,
// sorted by path so a sync pass is deterministic.
package plans

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/platform/github"
)

// planFile is one fetched plan markdown file: its repo-relative path and the
// verbatim bytes (decoded by the github client).
type planFile struct {
	Path    string
	Content string
}

// resolveSource turns an app row + its registry plans_source into the
// (owner, repo, path) triple the contents API walks. central_repo plans sit
// at the repo root; repo_path plans under the configured directory.
func resolveSource(owner, repo string, ps *app.PlansSource) (srcOwner, srcRepo, srcPath string, err error) {
	if ps == nil {
		return "", "", "", fmt.Errorf("plans: app %s/%s has no plans_source configured", owner, repo)
	}
	switch ps.Kind {
	case "repo_path":
		return owner, repo, strings.Trim(ps.Path, "/"), nil
	case "central_repo":
		slug := strings.Trim(ps.Repo, "/")
		o, r, ok := strings.Cut(slug, "/")
		if !ok || o == "" || r == "" {
			return "", "", "", fmt.Errorf("plans: central_repo %q: must be owner/repo", slug)
		}
		return o, r, "", nil
	default:
		return "", "", "", fmt.Errorf("plans: unknown plans_source kind %q", ps.Kind)
	}
}

// listPlanFiles walks dir (recursive, one contents-API level per call,
// mirroring corpus walkSpecs) and fetches every `.md` file at ref. Files come
// back sorted by path; a missing directory yields no plans (not an error —
// an app may not have authored plans yet).
func listPlanFiles(ctx context.Context, client *github.Client, owner, repo, dir, ref string) ([]planFile, error) {
	var files []planFile
	if err := walkPlans(ctx, client, owner, repo, dir, ref, &files); err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// walkPlans BFS-walks dir collecting .md files.
func walkPlans(ctx context.Context, client *github.Client, owner, repo, dir, ref string, out *[]planFile) error {
	entries, err := client.ListDir(ctx, owner, repo, ref, dir)
	if err != nil {
		if dir == "" {
			return fmt.Errorf("plans: list root of %s/%s: %w", owner, repo, err)
		}
		return fmt.Errorf("plans: list %s: %w", dir, err)
	}
	for _, entry := range entries {
		switch {
		case entry.Type == "dir":
			if err := walkPlans(ctx, client, owner, repo, entry.Path, ref, out); err != nil {
				return err
			}
		case entry.Type == "file" && strings.HasSuffix(entry.Name, ".md"):
			content, err := client.GetFile(ctx, owner, repo, ref, entry.Path)
			if err != nil {
				return fmt.Errorf("plans: fetch %s: %w", entry.Path, err)
			}
			*out = append(*out, planFile{Path: entry.Path, Content: content})
		}
	}
	return nil
}
