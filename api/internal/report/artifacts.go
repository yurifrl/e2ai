// T11: traversal-safe artifact file serving — GET /artifacts/{runID}/{path…}
// resolves strictly inside <E2AI_ARTIFACTS_DIR>/<runID>/ and streams the
// bytes. Access is token- or identity-gated (the PR comment embeds signed
// screenshot URLs; screenshots are the contract's token-carrying surface).
package report

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// ServeArtifact streams one artifact file under the fixed volume root.
// Traversal is rejected on every axis: "../" segments, absolute paths,
// URL-encoded dots, and symlink escape (EvalSymlinks + containment check).
func (s *RunsHTTP) ServeArtifact(w http.ResponseWriter, r *http.Request) {
	runRaw := r.PathValue("runID")
	rest := r.PathValue("path")
	if runRaw == "" || rest == "" {
		writeErr(w, http.StatusNotFound, "artifact not found")
		return
	}

	// URL-encoded traversal survives path cleaning in the mux; re-check the
	// raw escaped form before any decode step (%2e encodes ".").
	escaped := strings.ToLower(r.URL.EscapedPath())
	if strings.Contains(escaped, "..") || strings.Contains(escaped, "%2e") {
		writeErr(w, http.StatusNotFound, "artifact not found")
		return
	}

	runID, err := uuid.Parse(runRaw)
	if err != nil {
		writeErr(w, http.StatusNotFound, "artifact not found")
		return
	}
	if !s.gate(w, r, runID) {
		return
	}

	rel, ok := sanitizeArtifactPath(rest)
	if !ok {
		writeErr(w, http.StatusNotFound, "artifact not found")
		return
	}
	if !s.serveRunFile(w, r, runID, rel) {
		writeErr(w, http.StatusNotFound, "artifact not found")
		return
	}
}

// serveRunFile resolves rel strictly inside the run's volume directory and
// streams the file; false means 404 (missing, traversal, or symlink escape).
func (s *RunsHTTP) serveRunFile(w http.ResponseWriter, r *http.Request, runID uuid.UUID, rel string) bool {
	root := filepath.Clean(s.Root)
	if root == "." || root == "/" || !filepath.IsAbs(root) {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}

	// The run directory itself must be a real directory under the root — a
	// symlinked run directory pointing elsewhere is an escape.
	resolvedRun, err := filepath.EvalSymlinks(filepath.Join(root, runID.String()))
	if err != nil {
		return false
	}
	if resolvedRun != filepath.Join(resolvedRoot, runID.String()) {
		return false
	}

	// The file tail must resolve strictly inside the trusted run directory:
	// Join+Clean of an escaped rel escapes the dir, EvalSymlinks reveals
	// symlink escapes — the prefix check below catches both.
	target := filepath.Join(resolvedRun, rel)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false
	}
	if resolved != resolvedRun &&
		!strings.HasPrefix(resolved, resolvedRun+string(os.PathSeparator)) {
		return false
	}

	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		return false
	}
	f, err := os.Open(resolved)
	if err != nil {
		return false
	}
	defer f.Close()

	w.Header().Set("Content-Type", artifactContentType(resolved))
	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
	return true
}

// artifactContentType maps an artifact file to its serving content type by
// extension (screenshots, HTML report, JSON results, zipped traces).
func artifactContentType(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".json":
		return "application/json"
	case ".zip":
		return "application/zip"
	case ".webm":
		return "video/webm"
	case ".mp4":
		return "video/mp4"
	case ".txt", ".log":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
