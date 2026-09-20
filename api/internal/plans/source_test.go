// Sourcing tests (PLAN-02): resolveSource is pure table-driven; listPlanFiles
// exercises the real contents-API shapes against the fake GitHub server so
// directory recursion, .md filtering, and 404 semantics match production.
package plans

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/platform/github"
)

func TestResolveSource(t *testing.T) {
	cases := []struct {
		name     string
		owner    string
		repo     string
		ps       *app.PlansSource
		o, r, p  string
		wantFail bool
	}{
		{name: "nil source", ps: nil, wantFail: true},
		{name: "repo_path", owner: "NSXBet", repo: "bet-app", ps: &app.PlansSource{Kind: "repo_path", Path: "e2ai/plans"},
			o: "NSXBet", r: "bet-app", p: "e2ai/plans"},
		{name: "repo_path trims slashes", owner: "NSXBet", repo: "bet-app", ps: &app.PlansSource{Kind: "repo_path", Path: "/e2ai/plans/"},
			o: "NSXBet", r: "bet-app", p: "e2ai/plans"},
		{name: "central_repo", ps: &app.PlansSource{Kind: "central_repo", Repo: "NSXBet/e2ai-plans"},
			o: "NSXBet", r: "e2ai-plans", p: ""},
		{name: "central_repo no slash", ps: &app.PlansSource{Kind: "central_repo", Repo: "e2ai-plans"}, wantFail: true},
		{name: "unknown kind", ps: &app.PlansSource{Kind: "s3"}, wantFail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, r, p, err := resolveSource(tc.owner, tc.repo, tc.ps)
			if tc.wantFail {
				if err == nil {
					t.Fatalf("resolveSource(%s) = %q/%q/%q, want error", tc.name, o, r, p)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSource(%s): %v", tc.name, err)
			}
			if o != tc.o || r != tc.r || p != tc.p {
				t.Errorf("resolveSource(%s) = (%q, %q, %q), want (%q, %q, %q)", tc.name, o, r, p, tc.o, tc.r, tc.p)
			}
		})
	}
}

// contentsFake serves the two endpoints listPlanFiles consumes:
// GET /repos/{owner}/{repo}/contents/{path}?ref=... returning a ContentsEntry
// array, and base64 content fetches for files. path->handler.
type contentsFake struct {
	t   *testing.T
	mux *http.ServeMux
	srv *httptest.Server
}

func newContentsFake(t *testing.T) *contentsFake {
	cf := &contentsFake{t: t, mux: http.NewServeMux()}
	cf.srv = httptest.NewServer(cf.mux)
	t.Cleanup(cf.srv.Close)
	return cf
}

func (cf *contentsFake) dir(t *testing.T, path string, entries string) {
	t.Helper()
	cf.mux.HandleFunc("GET "+contentsPath(t, path), func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ref"); got != "main" {
			t.Errorf("list %s: ref = %q, want main", path, got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(entries))
	})
}

func (cf *contentsFake) file(t *testing.T, path, content string) {
	t.Helper()
	cf.mux.HandleFunc("GET "+contentsPath(t, path), func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"type":"file","name":%q,"path":%q,"content":%q,"encoding":"base64"}`,
			nameOf(path), path, base64.StdEncoding.EncodeToString([]byte(content)))
	})
}

func (cf *contentsFake) missing(t *testing.T, path string) {
	t.Helper()
	cf.mux.HandleFunc("GET "+contentsPath(t, path), func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
}

// contentsPath mirrors github.Client's endpoint construction
// (repos/o/r/contents/{path-escaped}?ref=) so the fake intercepts the real call.
func contentsPath(t *testing.T, path string) string {
	t.Helper()
	if path == "" {
		return "/repos/NSXBet/bet-app/contents/"
	}
	return "/repos/NSXBet/bet-app/contents/" + url.PathEscape(strings.Trim(path, "/"))
}

func nameOf(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func TestListPlanFiles(t *testing.T) {
	fake := newContentsFake(t)
	fake.dir(t, "e2ai/plans", `[
		{"type":"file","name":"a.md","path":"e2ai/plans/a.md"},
		{"type":"dir","name":"nested","path":"e2ai/plans/nested"},
		{"type":"file","name":"notes.txt","path":"e2ai/plans/notes.txt"}
	]`)
	fake.dir(t, "e2ai/plans/nested", `[{"type":"file","name":"b.md","path":"e2ai/plans/nested/b.md"}]`)
	fake.file(t, "e2ai/plans/a.md", "A plan")
	fake.file(t, "e2ai/plans/nested/b.md", "B plan")

	client, err := github.NewWithBaseURL("t", fake.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	files, err := listPlanFiles(context.Background(), client, "NSXBet", "bet-app", "e2ai/plans", "main")
	if err != nil {
		t.Fatalf("listPlanFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files (%+v), want 2 — .md filter or recursion broken", len(files), files)
	}
	if files[0].Path != "e2ai/plans/a.md" || files[1].Path != "e2ai/plans/nested/b.md" {
		t.Errorf("paths = %q, %q — not sorted by path", files[0].Path, files[1].Path)
	}
	if string(files[0].Content) != "A plan" {
		t.Errorf("content = %q, want decoded bytes", files[0].Content)
	}
}

func TestListPlanFilesEmptyDir(t *testing.T) {
	fake := newContentsFake(t)
	fake.dir(t, "e2ai/plans", `[]`)
	client, err := github.NewWithBaseURL("t", fake.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	files, err := listPlanFiles(context.Background(), client, "NSXBet", "bet-app", "e2ai/plans", "main")
	if err != nil {
		t.Fatalf("empty dir should not error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("got %d files, want 0", len(files))
	}
}

func TestListPlanFilesMissingDir(t *testing.T) {
	fake := newContentsFake(t)
	fake.missing(t, "e2ai/plans")
	client, err := github.NewWithBaseURL("t", fake.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = listPlanFiles(context.Background(), client, "NSXBet", "bet-app", "e2ai/plans", "main")
	if err == nil {
		t.Fatal("missing declared plans dir must error — silent drop would hide config bugs")
	}
	if !strings.Contains(err.Error(), "e2ai/plans") {
		t.Errorf("error should name the failed directory: %v", err)
	}
}
