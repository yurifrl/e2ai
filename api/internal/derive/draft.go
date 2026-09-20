// draft.go — deterministic draft plan generation (DERV-02). The Phase 2
// generator stands in for the P3 Plan Author agent: for every mapped plan
// it renders a bumped-version derived plan seeded from the verified corpus
// behaviors whose provenance overlaps the changed paths (the context's K
// slice), one falsifiable step per behavior, always with sources:
// provenance that is never diff-only (the tautology guard, DERV-03).
// Rendering is pure bytes and stores nothing; storage goes through the
// plans draft-upsert seam so drafts land as draft status, never
// pending_approval without a human.
package derive

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/NSXBet/e2ai/api/internal/corpus"
	"github.com/NSXBet/e2ai/api/internal/plans"
)

// versionTail matches a plan id's `-vX.Y.Z` suffix (same shape the plans
// package's planVersion helper reads).
var versionTail = regexp.MustCompile(`-v(\d+)\.(\d+)\.(\d+)$`)

// DraftPath is the source path a derived draft stores under: distinct from
// the synced tree so the plans UI can show where a version came from.
func DraftPath(planID string) string {
	return "e2ai/derived/" + planID + ".md"
}

// BumpVersion bumps the patch component of a plan id's `-vX.Y.Z` tail:
// derived drafts iterate the same version space the plans package reads
// (planVersion) without colliding with the parent's hash identity. An id
// with no version tail gets v0.1.0 — a bump from the implicit "0".
func BumpVersion(planID string) string {
	m := versionTail.FindStringSubmatchIndex(planID)
	if m == nil {
		return planID + "-v0.1.0"
	}
	patch, _ := strconv.Atoi(planID[m[6]:m[7]])
	return planID[:m[0]] + fmt.Sprintf("-v%s.%s.%d", planID[m[2]:m[3]], planID[m[4]:m[5]], patch+1)
}

// DraftSpec is the rendered draft's inputs: the parent plan's identity and
// the verified behaviors the diff overlapped (already filtered by the
// caller's scope mapping).
type DraftSpec struct {
	ParentPlanID string
	AppRef       string
	Title        string
	Surfaces     []string
	Trigger      string
	Behaviors    []corpus.Behavior // verified only, provenance-overlap order
}

// Render renders a derived draft plan document from the spec. Determinism:
// the same context bytes render byte-identical output, so a re-derivation
// is a content-hash no-op. Round-trips through plans.Parse so a rendering
// bug fails here, not at store time.
func Render(spec DraftSpec) (string, error) {
	id := BumpVersion(spec.ParentPlanID)
	content := render(id, spec)
	if _, err := plans.Parse([]byte(content)); err != nil {
		return "", fmt.Errorf("derive: rendered draft does not round-trip: %w", err)
	}
	return content, nil
}

func render(id string, spec DraftSpec) string {
	content := "---\n" +
		"id: " + id + "\n" +
		"app: " + spec.AppRef + "\n" +
		"title: " + strconv.Quote(spec.Title) + "\n" +
		"surfaces: [" + strings.Join(spec.Surfaces, ", ") + "]\n" +
		"trigger: " + spec.Trigger + "\n" +
		"derived: true\n" +
		"sources: [" + strings.Join(spec.sources(), ", ") + "]\n" +
		"---\n"
	for i, b := range spec.Behaviors {
		content += fmt.Sprintf("%d. Exercise the behavior pinned by corpus key %s: %s\n",
			i+1, b.Key, b.Title)
		content += "   judge: " + b.Description + "\n"
		content += "   uses: " + b.Key + "\n"
	}
	return content
}

// sources derives the provenance list: one corpus:<key> per seeded
// behavior. Never diff-derived on its own — with zero overlapping verified
// behaviors there is nothing to draft, so the list is non-empty by
// construction.
func (s DraftSpec) sources() []string {
	out := make([]string, 0, len(s.Behaviors))
	for _, b := range s.Behaviors {
		out = append(out, "corpus:"+b.Key)
	}
	return out
}
