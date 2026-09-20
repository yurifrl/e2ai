// Package derive turns PR diffs into test-plan scope and draft plan
// versions (DERV-01..03). Scope mapping is pure: changed paths in, ranked
// plan selection out. Draft generation renders a valid plans-format
// document from the derivation context C = {D, K, S, B} (research §Pattern
// 2) and stores it through the plans draft-upsert seam — always with
// status draft, never pending_approval without a human approval.
package derive

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/NSXBet/e2ai/api/internal/corpus"
	"github.com/gobwas/glob"
)

// Priority classes for a mapped plan. Lower sorts first.
const (
	PriorityDirect   = 1 // the plan's pr-filter (or absence of one) matched a changed path
	PriorityIndirect = 2 // the plan uses a corpus behavior whose provenance overlaps a changed path
)

// PlanInput is the derivation-relevant slice of one synced plan: its pr-filter
// globs and the corpus behavior keys its steps reference.
type PlanInput struct {
	ID       string
	PRFilter []string // empty pr-filter = plan runs on every deploy (always direct)
	Uses     []string
}

// BehaviorInput is one corpus behavior with the provenance the indirect
// mapping rule reads (a playwright-spec behavior pins its source path).
type BehaviorInput struct {
	Key        string
	Provenance corpus.Provenance
}

// MappedPlan is one plan's scope verdict.
type MappedPlan struct {
	PlanID   string
	Priority int      // PriorityDirect or PriorityIndirect
	Reasons  []string // human-readable evidence for the queue UI
}

// LastFailedAt supplies the most-recent-failed-verdict tiebreak: plan id →
// finish time of the plan's latest failed run. The runs↔plan linkage does
// not exist in the schema yet (0001/0002 carry no plan_id on runs — the
// per-test results join lands in the scheduler phase), so the caller
// supplies it. Absent entries sort last within their priority class.
type LastFailedAt map[string]time.Time

// MapScope assigns each plan a priority class and returns the mapped ones,
// ordered by priority, then most-recent failed verdict, then plan id.
//
// Rules:
//
//  1. Direct — pr-filter matches any changed path (gobwas/glob `**` globs).
//     A plan with no pr-filter applies to every deploy → direct.
//  2. Indirect — no direct match, but a corpus behavior the plan `uses:`
//     has playwright-spec provenance whose path overlaps a changed path.
//     Only the watch-path overlap counts: verified-behavior reuse alone
//     (same behavior key referenced without any source-path change) is not
//     a diff signal, or every plan sharing the oracle would always fire.
//  3. Not mapped — the plan is out of scope for this diff.
func MapScope(plans []PlanInput, behaviors []BehaviorInput, changedPaths []string, lastFailed LastFailedAt) []MappedPlan {
	// Indirect lookup: behavior key → spec provenance path.
	specPath := map[string]string{}
	for _, b := range behaviors {
		if b.Provenance.Kind == corpus.ProvenancePlaywrightSpec && b.Provenance.Path != "" {
			specPath[b.Key] = b.Provenance.Path
		}
	}

	var mapped []MappedPlan
	for _, p := range plans {
		if m, ok := mapDirect(p, changedPaths); ok {
			mapped = append(mapped, m)
			continue
		}
		if m, ok := mapIndirect(p, specPath, changedPaths); ok {
			mapped = append(mapped, m)
		}
	}

	sort.Slice(mapped, func(i, j int) bool {
		if mapped[i].Priority != mapped[j].Priority {
			return mapped[i].Priority < mapped[j].Priority
		}
		fi, fj := lastFailed[mapped[i].PlanID], lastFailed[mapped[j].PlanID]
		if !fi.Equal(fj) {
			return fi.After(fj) // zero time sorts last
		}
		return mapped[i].PlanID < mapped[j].PlanID
	})
	return mapped
}

// mapDirect reports whether the plan is in direct scope for the changed
// paths. An empty pr-filter matches every deploy.
func mapDirect(p PlanInput, changedPaths []string) (MappedPlan, bool) {
	if len(p.PRFilter) == 0 {
		return MappedPlan{PlanID: p.ID, Priority: PriorityDirect,
			Reasons: []string{"no pr-filter: applies to every deploy"}}, true
	}
	reasons := []string{}
	for _, pattern := range p.PRFilter {
		m, err := glob.Compile(pattern)
		if err != nil {
			continue // malformed globs are rejected at ingest; belt and braces
		}
		for _, path := range changedPaths {
			if m.Match(path) {
				reasons = append(reasons, fmt.Sprintf("pr-filter %q matches changed path %q", pattern, path))
			}
		}
	}
	if len(reasons) == 0 {
		return MappedPlan{}, false
	}
	return MappedPlan{PlanID: p.ID, Priority: PriorityDirect, Reasons: reasons}, true
}

// mapIndirect reports whether the plan is in indirect scope: one of its
// referenced behaviors has playwright-spec provenance overlapping a changed
// path.
func mapIndirect(p PlanInput, specPath map[string]string, changedPaths []string) (MappedPlan, bool) {
	reasons := []string{}
	for _, key := range p.Uses {
		path, ok := specPath[key]
		if !ok {
			continue
		}
		for _, changed := range changedPaths {
			if overlap(path, changed) {
				reasons = append(reasons, fmt.Sprintf("uses %s sourced from %q, which overlaps changed path %q", key, path, changed))
			}
		}
	}
	if len(reasons) == 0 {
		return MappedPlan{}, false
	}
	return MappedPlan{PlanID: p.ID, Priority: PriorityIndirect, Reasons: reasons}, true
}

// overlap reports whether two repo paths overlap: either is a prefix of the
// other at a `/` boundary (a directory change covers its files and vice
// versa).
func overlap(a, b string) bool {
	if a == b {
		return true
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	return len(long) > len(short) && long[len(short)] == '/' &&
		long[:len(short)] == short
}

// DiffSource supplies the changed-path set. It is narrow so tests fake it:
// the PR branch is preferred (per-PR files endpoint, no compare caps), the
// compare branch falls back to base...head and reports whether the response
// was truncated by the API's caps.
type DiffSource interface {
	// ChangedPaths returns the changed paths for (base, head); the bool is
	// the truncation flag, true only for the compare fallback when the
	// compare API hit its documented caps.
	ChangedPaths(ctx context.Context, base, head string) (paths []string, truncated bool, err error)
}
