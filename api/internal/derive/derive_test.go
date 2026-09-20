package derive

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NSXBet/e2ai/api/internal/corpus"
	"github.com/NSXBet/e2ai/api/internal/plans"
	"github.com/NSXBet/e2ai/api/internal/platform/github"
)

// --- scope mapping (pure) ---

func TestMapScopeDirect(t *testing.T) {
	pls := []PlanInput{
		{ID: "checkout-v1.0.0", PRFilter: []string{"src/checkout/**"}},
		{ID: "always-v1.0.0"}, // no pr-filter: applies to every deploy
	}
	mapped := MapScope(pls, nil, []string{"src/checkout/cart.ts"}, nil)
	if len(mapped) != 2 {
		t.Fatalf("mapped = %d, want 2", len(mapped))
	}
	if mapped[0].PlanID != "always-v1.0.0" || mapped[1].PlanID != "checkout-v1.0.0" {
		t.Fatalf("order = %s,%s; no-filter plan ties with filter match, id decides", mapped[0].PlanID, mapped[1].PlanID)
	}
	for _, m := range mapped {
		if m.Priority != PriorityDirect {
			t.Fatalf("%s priority = %d, want direct", m.PlanID, m.Priority)
		}
	}
}

func TestMapScopeDirectGlob(t *testing.T) {
	pls := []PlanInput{{ID: "odds-v1.0.0", PRFilter: []string{"e2ai/plans/*.md"}}}
	mapped := MapScope(pls, nil, []string{"e2ai/plans/checkout.md"}, nil)
	if len(mapped) != 1 || mapped[0].Priority != PriorityDirect {
		t.Fatalf("glob pr-filter did not match: %+v", mapped)
	}
	if mapped := MapScope(pls, nil, []string{"src/other.ts"}, nil); len(mapped) != 0 {
		t.Fatalf("non-matching glob mapped: %+v", mapped)
	}
}

func TestMapScopeIndirect(t *testing.T) {
	// A pr-filter that does not match the changed paths keeps the plan out
	// of direct scope, so the mapping must come from the behavior overlap.
	pls := []PlanInput{{ID: "badge-v1.0.0", PRFilter: []string{"unrelated/*.md"}, Uses: []string{"PWSPEC-LIVE-BADGE"}}}
	behaviors := []BehaviorInput{
		{Key: "PWSPEC-LIVE-BADGE", Provenance: corpus.Provenance{
			Kind: corpus.ProvenancePlaywrightSpec, Path: "web/tests/live-badge.spec.ts",
		}},
		{Key: "PWSPEC-ODDS", Provenance: corpus.Provenance{
			Kind: corpus.ProvenancePlaywrightSpec, Path: "web/tests/odds.spec.ts",
		}},
	}
	mapped := MapScope(pls, behaviors, []string{"web/tests/live-badge.spec.ts"}, nil)
	if len(mapped) != 1 || mapped[0].Priority != PriorityIndirect {
		t.Fatalf("indirect overlap not mapped: %+v", mapped)
	}
	if mapped := MapScope(pls, behaviors, []string{"web/tests/odds.spec.ts"}, nil); len(mapped) != 0 {
		t.Fatalf("behavior referenced but not sourced from the changed path still mapped: %+v", mapped)
	}
	// Reuse without source-path change is not a diff signal: the plan uses a
	// behavior, but the changed path touches nothing its provenance covers.
	mapped = MapScope(pls, behaviors, []string{"src/badge/logo.ts"}, nil)
	if len(mapped) != 0 {
		t.Fatalf("unrelated source change mapped indirect: %+v", mapped)
	}
}

func TestMapScopeExcluded(t *testing.T) {
	pls := []PlanInput{{ID: "desktop-v1.0.0", PRFilter: []string{"desktop/**"}}}
	if mapped := MapScope(pls, nil, []string{"web/app/page.tsx"}, nil); len(mapped) != 0 {
		t.Fatalf("out-of-scope plan mapped: %+v", mapped)
	}
}

func TestMapScopePriorityAndTiebreak(t *testing.T) {
	direct, indirect := "direct-v1.0.0", "indirect-v1.0.0"
	pls := []PlanInput{
		{ID: indirect, Uses: []string{"PWSPEC-ODDS"}},
		{ID: direct, PRFilter: []string{"web/**"}},
	}
	behaviors := []BehaviorInput{{Key: "PWSPEC-ODDS", Provenance: corpus.Provenance{
		Kind: corpus.ProvenancePlaywrightSpec, Path: "web/tests/odds.spec.ts",
	}}}
	mapped := MapScope(pls, behaviors, []string{"web/tests/odds.spec.ts"}, nil)
	if len(mapped) != 2 || mapped[0].PlanID != direct || mapped[1].PlanID != indirect {
		t.Fatalf("direct must outrank indirect: %+v", mapped)
	}

	// Same class: most-recent failed verdict wins, then plan id.
	pls = []PlanInput{{ID: "a-v1.0.0", PRFilter: []string{"web/**"}}, {ID: "b-v1.0.0", PRFilter: []string{"web/**"}}, {ID: "c-v1.0.0", PRFilter: []string{"web/**"}}}
	recent := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	mapped = MapScope(pls, nil, []string{"web/x.ts"}, LastFailedAt{"b-v1.0.0": recent})
	if mapped[0].PlanID != "b-v1.0.0" {
		t.Fatalf("most-recent failure did not win: %+v", mapped)
	}
	// Never-failed plans (zero time) sort last; then by id.
	mapped = MapScope(pls, nil, []string{"web/x.ts"}, LastFailedAt{"c-v1.0.0": recent})
	if mapped[0].PlanID != "c-v1.0.0" || mapped[1].PlanID != "a-v1.0.0" || mapped[2].PlanID != "b-v1.0.0" {
		t.Fatalf("tiebreak order wrong: %+v", mapped)
	}
}

// --- version bump + rendering ---

func TestBumpVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"checkout-flow-v1.2.3", "checkout-flow-v1.2.4"},
		{"plain", "plain-v0.1.0"},
		{"a-v2.0.9", "a-v2.0.10"},
	}
	for _, c := range cases {
		if got := BumpVersion(c.in); got != c.want {
			t.Errorf("BumpVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func testSpec() DraftSpec {
	return DraftSpec{
		ParentPlanID: "checkout-flow-v1.0.0",
		AppRef:       "NSXBet/bet-app",
		Title:        "Derived: checkout flow",
		Surfaces:     []string{"web"},
		Trigger:      plans.TriggerOnDeploy,
		Behaviors: []corpus.Behavior{
			{Key: "MATCH-LIVE-BADGE", Title: "Live badge", Description: "the live badge is visible"},
			{Key: "PWSPEC-ODDS", Title: "Odds panel", Description: "odds match at least 1 market"},
		},
	}
}

func TestRenderDeterministicAndRoundTrips(t *testing.T) {
	spec := testSpec()
	first, err := Render(spec)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := Render(spec)
	if err != nil {
		t.Fatalf("render again: %v", err)
	}
	if first != second {
		t.Fatal("render is not deterministic — identical contexts must render byte-identical plans")
	}
	plan, err := plans.Parse([]byte(first))
	if err != nil {
		t.Fatalf("rendered draft does not parse: %v\n%s", err, first)
	}
	if !plan.Frontmatter.Derived {
		t.Fatal("draft frontmatter lacks derived: true")
	}
	if plan.Frontmatter.App != spec.AppRef {
		t.Fatalf("app = %q", plan.Frontmatter.App)
	}
	wantID := "checkout-flow-v1.0.1"
	if plan.Frontmatter.ID != wantID {
		t.Fatalf("draft id = %q, want bumped %q", plan.Frontmatter.ID, wantID)
	}
}

func TestRenderSourcesNeverDiffOnly(t *testing.T) {
	spec := testSpec()
	content, err := Render(spec)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	plan, err := plans.Parse([]byte(content))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(plan.Frontmatter.Sources) != len(spec.Behaviors) {
		t.Fatalf("sources = %v", plan.Frontmatter.Sources)
	}
	for i, s := range plan.Frontmatter.Sources {
		if s != "corpus:"+spec.Behaviors[i].Key {
			t.Errorf("source[%d] = %q, want corpus:<key>", i, s)
		}
		if plans.IsDiffSource(s) {
			t.Errorf("source %q is diff-derived — tautology guard would reject", s)
		}
	}
	// One step per behavior, each anchored with uses: so the judge check is
	// falsifiable and the steps resolve to verified behaviors at ingest.
	if len(plan.Steps) != len(spec.Behaviors) {
		t.Fatalf("steps = %d, want one per behavior", len(plan.Steps))
	}
	for i, step := range plan.Steps {
		if len(step.Uses) != 1 || step.Uses[0] != spec.Behaviors[i].Key {
			t.Errorf("step %d uses = %v, want [%s]", step.Number, step.Uses, spec.Behaviors[i].Key)
		}
		if step.Judge == "" {
			t.Errorf("step %d has no judge", step.Number)
		}
	}
	if !strings.Contains(content, "judge: the live badge is visible") {
		t.Errorf("behavior description not rendered as judge:\n%s", content)
	}
}

// --- diff preference rule (fake github client) ---

type fakeGH struct {
	pulls        []github.CommitPull
	pullsCalled  bool
	compareCall  int
	prFilesCalls []int
	compareRes   github.DiffResult
	compareErr   error
	prFiles      []github.CompareFile
}

func (f *fakeGH) ListPullsForCommit(ctx context.Context, owner, repo, sha string) ([]github.CommitPull, error) {
	f.pullsCalled = true
	return f.pulls, nil
}

func (f *fakeGH) Compare(ctx context.Context, owner, repo, base, head string) (github.DiffResult, error) {
	f.compareCall++
	return f.compareRes, f.compareErr
}

func (f *fakeGH) ListPRFiles(ctx context.Context, owner, repo string, number int) ([]github.CompareFile, error) {
	f.prFilesCalls = append(f.prFilesCalls, number)
	return f.prFiles, nil
}

func TestChangedPathsResolvedPrefersPR(t *testing.T) {
	fake := &fakeGH{
		pulls: []github.CommitPull{{Number: 7, State: "open"}},
		prFiles: []github.CompareFile{
			{Filename: "web/tests/live-badge.spec.ts"},
		},
	}
	d := Diff{Client: fake, Owner: "NSXBet", Repo: "bet-app"}

	paths, truncated, pr, err := d.ChangedPathsResolved(context.Background(), "main", "feature", "abc", []int{12})
	if err != nil {
		t.Fatalf("resolved: %v", err)
	}
	if pr != 12 || len(paths) != 1 || paths[0] != "web/tests/live-badge.spec.ts" || truncated {
		t.Fatalf("webhook PR should win: pr=%d paths=%v truncated=%v", pr, paths, truncated)
	}
	if fake.pullsCalled || fake.compareCall != 0 {
		t.Fatal("webhook PR known must not consult commit resolution or compare")
	}

	// Empty webhook list (workflow_dispatch): resolve from the head SHA.
	fake2 := &fakeGH{pulls: []github.CommitPull{{Number: 7, State: "open"}},
		prFiles: []github.CompareFile{{Filename: "src/a.ts"}}}
	d2 := Diff{Client: fake2, Owner: "NSXBet", Repo: "bet-app"}
	paths, truncated, pr, err = d2.ChangedPathsResolved(context.Background(), "main", "feature", "abc", nil)
	if err != nil || pr != 7 || !fake2.pullsCalled || fake2.compareCall != 0 || truncated {
		t.Fatalf("head-SHA resolution failed: pr=%d err=%v calls=%v/%v", pr, err, fake2.pullsCalled, fake2.compareCall)
	}
	if len(paths) != 1 || paths[0] != "src/a.ts" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestChangedPathsResolvedFallsBackToCompare(t *testing.T) {
	fake := &fakeGH{compareRes: github.DiffResult{
		Files:         []github.CompareFile{{Filename: "web/a.ts"}, {Filename: "web/b.ts"}},
		DiffTruncated: true,
	}}
	d := Diff{Client: fake, Owner: "NSXBet", Repo: "bet-app"}
	paths, truncated, pr, err := d.ChangedPathsResolved(context.Background(), "main", "feature", "", nil)
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if pr != 0 || !truncated || len(paths) != 2 {
		t.Fatalf("compare fallback wrong: pr=%d truncated=%v paths=%v", pr, truncated, paths)
	}
	if fake.compareCall != 1 {
		t.Fatal("compare not called on the no-PR path")
	}

	// A PR exists but is closed and nothing open: still per-PR resolution
	// (first entry) per the preference rule.
	fake2 := &fakeGH{pulls: []github.CommitPull{{Number: 3, State: "closed"}},
		prFiles: []github.CompareFile{{Filename: "src/c.ts"}}}
	d2 := Diff{Client: fake2, Owner: "NSXBet", Repo: "bet-app"}
	_, _, pr, err = d2.ChangedPathsResolved(context.Background(), "main", "feature", "abc", nil)
	if err != nil || pr != 3 || fake2.compareCall != 0 {
		t.Fatalf("closed-PR resolution wrong: pr=%d err=%v", pr, err)
	}

	// Errors propagate.
	fake3 := &fakeGH{compareErr: errors.New("boom")}
	d3 := Diff{Client: fake3, Owner: "NSXBet", Repo: "bet-app"}
	if _, _, _, err := d3.ChangedPathsResolved(context.Background(), "main", "feature", "", nil); err == nil {
		t.Fatal("compare error swallowed")
	}
}
