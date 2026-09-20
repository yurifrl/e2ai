// Tests for T11's traversal-safe artifact serving: every escape axis
// (../, absolute, URL-encoded dots, symlink file/run-dir escape) is rejected,
// content types map by extension, bytes stream, and access is token-gated.
package report

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// artifactSurface builds a mux + surface rooted at the given volume dir.
func artifactSurface(t *testing.T, pool *pgxpool.Pool, root, secret string) http.Handler {
	t.Helper()
	surface := NewRunsHTTP(pool, root, secret)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /artifacts/{runID}/{path...}", surface.ServeArtifact)
	return mux
}

// volume lays out a run's artifact directory with one screenshot.
func volume(t *testing.T) (root string, runID uuid.UUID) {
	t.Helper()
	root = t.TempDir()
	runID = randomUUID()
	runDir := filepath.Join(root, runID.String())
	if err := os.MkdirAll(filepath.Join(runDir, "screenshots"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "screenshots", "shot.png"),
		[]byte("\x89PNG\r\n\x1a\nfakebytes"), 0o644); err != nil {
		t.Fatalf("write screenshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "results.json"),
		[]byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatalf("write results: %v", err)
	}
	return root, runID
}

// serveArtifactPool is a nil-pool surface: the artifact path needs no DB.
func artifactPool(t *testing.T) *pgxpool.Pool {
	return nil
}

// TestArtifactTraversalRejected walks every traversal axis — dot segments,
// URL-encoded dots, absolute paths, backslashes, symlink escape — and asserts
// 404 without touching anything outside the volume root.
func TestArtifactTraversalRejected(t *testing.T) {
	root, runID := volume(t)
	surface := NewRunsHTTP(nil, root, "")
	handler := artifactSurface(t, nil, root, "")

	// Raw-path cases: ServeMux cleans unencoded dot segments with a 307
	// redirect before dispatch, so the resolver is exercised directly —
	// each rel must be rejected with 404 and never touch the volume.
	cases := []string{
		"screenshots/../../escape.png",
		"../../etc/passwd",
		"/etc/passwd",
		"screenshots/shot.png/../../../etc/passwd",
		`screenshots\shot.png`,
		"",
		"missing.png",
	}
	for i, rel := range cases {
		rel := rel
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/artifacts/"+runID.String()+"/probe", nil)
			if surface.serveRunFile(rec, req, runID, rel) {
				t.Errorf("path %q: resolver accepted the request", rel)
			}
			// The resolver's contract is "false = reject"; ServeArtifact
			// (mux-level cases below) turns that into the 404 write.
		})
	}

	// URL-encoded traversal survives ServeMux cleaning (the mux never
	// decodes %2e/%2f), so these run through the full mounted handler.
	for _, path := range []string{
		"..%2Fscreenshots/shot.png",
		"screenshots/%2e%2e/escape.png",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET",
			"/artifacts/"+runID.String()+"/"+path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %q: status %d want 404", path, rec.Code)
		}
	}

	// Symlinked file pointing outside the run dir is an escape.
	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, []byte("leak"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	link := filepath.Join(root, runID.String(), "screenshots", "leak.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/artifacts/"+runID.String()+"/screenshots/leak.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("symlinked file: status %d want 404", rec.Code)
	}

	// Symlinked run directory pointing elsewhere is an escape too.
	other := t.TempDir()
	os.Symlink(other, filepath.Join(root, randomUUID().String()+"-skip")) // noise
	stolenID := randomUUID()
	if err := os.Symlink(other, filepath.Join(root, stolenID.String())); err != nil {
		t.Fatalf("symlink run dir: %v", err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/artifacts/"+stolenID.String()+"/anything.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("symlinked run dir: status %d want 404", rec.Code)
	}

	// Bad run uuid segment is a 404, not a resolve.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/artifacts/not-a-uuid/screenshots/shot.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("bad run uuid: status %d want 404", rec.Code)
	}
}

// TestArtifactServesBytes checks the happy path: real bytes, correct content
// type, and a 404 for files missing inside a valid run dir.
func TestArtifactServesBytes(t *testing.T) {
	root, runID := volume(t)
	handler := artifactSurface(t, artifactPool(t), root, "")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/artifacts/"+runID.String()+"/screenshots/shot.png", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("screenshot: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("content type: got %s want image/png", ct)
	}
	if rec.Body.Len() == 0 || rec.Body.Bytes()[0] != 0x89 {
		t.Errorf("body: %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/artifacts/"+runID.String()+"/results.json", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("results.json: %d ct=%s", rec.Code, rec.Header().Get("Content-Type"))
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/artifacts/"+runID.String()+"/nope.json", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing file: %d want 404", rec.Code)
	}
}

// TestArtifactContentTypeTable pins the extension → content-type map.
func TestArtifactContentTypeTable(t *testing.T) {
	for ext, want := range map[string]string{
		".png":   "image/png",
		".jpg":   "image/jpeg",
		".webp":  "image/webp",
		".html":  "text/html; charset=utf-8",
		".json":  "application/json",
		".zip":   "application/zip",
		".webm":  "video/webm",
		".log":   "text/plain; charset=utf-8",
		".weird": "application/octet-stream",
		".TRACE": "application/octet-stream",
		".ZIP":   "application/zip",
	} {
		if got := artifactContentType("x" + ext); got != want {
			t.Errorf("ext %s: got %s want %s", ext, got, want)
		}
	}
}

// TestArtifactGate proves token-only access: a valid ?e2ai= token reaches
// the file without any identity, a bad token is rejected.
func TestArtifactGate(t *testing.T) {
	secret := "artifact-gate-secret"
	root, runID := volume(t)
	handler := artifactSurface(t, artifactPool(t), root, secret)

	token := reportToken(runID, secret)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET",
		"/artifacts/"+runID.String()+"/screenshots/shot.png?e2ai="+token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("token access: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET",
		"/artifacts/"+runID.String()+"/screenshots/shot.png?e2ai=bogus", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token: %d want 401", rec.Code)
	}

	// No token, no identity → 401.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET",
		"/artifacts/"+runID.String()+"/screenshots/shot.png", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d want 401", rec.Code)
	}
}
