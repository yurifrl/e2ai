// Seed tests: the pure parser (no database) plus the full seed path against
// a fake GitHub contents API (httptest) and a real scratch Postgres, proving
// idempotency across re-runs.
package corpus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NSXBet/e2ai/api/internal/platform/github"
)

func TestParseSpec(t *testing.T) {
	spec := `import { test, expect } from '@playwright/test';

test.describe('live odds', () => {
  test('shows the live badge', async ({ page }) => {
    await page.goto('/live');
    await expect(page.getByTestId('live-badge')).toBeVisible();
  });

  test('renders odds grid "quotes"', async ({ page }) => {
    await expect(page.locator('.grid'), 'with quotes)').toHaveCount(1);
    await expect(page.getByText('Ready')).toBeVisible();
  });

  test('title only, no asserts', async ({ page }) => {
    await page.goto('/scratch');
  });
});
`
	tests := parseSpec(spec)
	if len(tests) != 3 {
		t.Fatalf("want 3 tests parsed, got %d: %+v", len(tests), tests)
	}
	if got := tests[0].Title(); got != "live odds > shows the live badge" {
		t.Errorf("title = %q", got)
	}
	if len(tests[0].Expects) != 1 || tests[0].Expects[0] != "expect(page.getByTestId('live-badge')).toBeVisible()" {
		t.Errorf("expects[0] = %v", tests[0].Expects)
	}
	// Title containing quotes must not break the matcher or the expects scan.
	if got := tests[1].Title(); got != `live odds > renders odds grid "quotes"` {
		t.Errorf("quoted title = %q", got)
	}
	if len(tests[1].Expects) != 2 ||
		tests[1].Expects[0] != "expect(page.locator('.grid')).toHaveCount(1)" ||
		tests[1].Expects[1] != "expect(page.getByText('Ready')).toBeVisible()" {
		t.Errorf("expects[1] = %v", tests[1].Expects)
	}
	if len(tests[2].Expects) != 0 {
		t.Errorf("no-expect test must seed on title alone, got %v", tests[2].Expects)
	}

	if key := behaviorKey(tests[0].Title()); key != "PWSPEC-LIVE-ODDS-SHOWS-THE-LIVE-BADGE" {
		t.Errorf("key = %q", key)
	}
	// Non-ascii separators collapse; long titles truncate deterministically.
	if key := behaviorKey("Renders odds grid \"quotes\""); key != "PWSPEC-RENDERS-ODDS-GRID-QUOTES" {
		t.Errorf("quoted key = %q", key)
	}
	if key := behaviorKey("a b/c" + dashes(80)); len(key) > len(BehaviorKeyPrefix)+1+64 {
		t.Errorf("key not truncated: %d", len(key))
	}
}

func dashes(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		s += " "
	}
	return s
}

func TestSeedSpecsIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	appID := testApp(t, pool)

	const commit = "f00dc0de"
	files := map[string]string{
		"e2e/live.spec.ts": `test.describe('live odds', () => {
  test('shows the live badge', async ({ page }) => {
    await expect(page.getByTestId('live-badge')).toBeVisible();
  });
});`,
		"e2e/pay/pay.spec.ts": `test('pays a bet slip', async ({ page }) => {
  await page.goto('/pay');
});`,
	}
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		rel := strings.Trim(strings.TrimPrefix(path, "/repos/nsx/bet/contents/"), "/")
		switch rel {
		case "e2e", "e2e/pay":
			hits["dir:"+rel+"/"]++
			if rel == "e2e" {
				writeContents(w, contentsEntry{Name: "live.spec.ts", Path: "e2e/live.spec.ts", Type: "file"},
					contentsEntry{Name: "pay", Path: "e2e/pay", Type: "dir"})
			} else {
				writeContents(w, contentsEntry{Name: "pay.spec.ts", Path: "e2e/pay/pay.spec.ts", Type: "file"})
			}
		default:
			if spec, ok := files[rel]; ok {
				hits["file:"+path]++
				writeFile(w, spec)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	client, err := github.NewWithBaseURL("token", srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	first, err := SeedSpecs(ctx, pool, client, appID, "nsx", "bet", "e2e/", commit)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if first.FilesScanned != 2 || first.Behaviors != 2 {
		t.Fatalf("first seed = %+v, want 2 files / 2 behaviors", first)
	}

	// Idempotency: second pass over the same repo state changes nothing.
	second, err := SeedSpecs(ctx, pool, client, appID, "nsx", "bet", "e2e/", commit)
	if err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if first != second {
		t.Fatalf("re-seed summary changed: first %+v second %+v", first, second)
	}
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from corpus_behaviors where app_id = $1`, appID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("want 2 behaviors total, got %d", count)
	}

	// Provenance + verified-by-seed invariants.
	badge, err := GetBehavior(ctx, pool, appID, "PWSPEC-LIVE-ODDS-SHOWS-THE-LIVE-BADGE")
	if err != nil || badge == nil {
		t.Fatalf("seeded behavior missing: %v %v", err, badge)
	}
	if badge.Status != BehaviorVerified {
		t.Errorf("spec-seeded behavior must be verified, got %q", badge.Status)
	}
	if badge.Provenance.Kind != ProvenancePlaywrightSpec || badge.Provenance.Path != "e2e/live.spec.ts" || badge.Provenance.Commit != commit {
		t.Errorf("provenance = %+v", badge.Provenance)
	}
	if badge.Description != "live odds > shows the live badge — expects: expect(page.getByTestId('live-badge')).toBeVisible()" {
		t.Errorf("description = %q", badge.Description)
	}

	// Empty specs dir must be a hard error (a silent partial oracle is worse).
	if _, err := SeedSpecs(ctx, pool, client, appID, "nsx", "bet", "missing/", commit); err == nil {
		t.Error("missing specs dir must fail the seed")
	}
}

func TestSeedSpecsFromLearnedBehavior(t *testing.T) {
	// Full round-trip in one app: seed spec behaviors, append a learning,
	// promote — the corpus then holds both provenance kinds.
	pool := testPool(t)
	ctx := context.Background()
	appID := testApp(t, pool)
	runID := testRun(t, pool, appID)

	l, err := AppendLearning(ctx, pool, appID, runID, "odds row shows stale price until refresh")
	if err != nil {
		t.Fatal(err)
	}
	b, err := PromoteLearning(ctx, pool, l.ID, "yuri")
	if err != nil {
		t.Fatal(err)
	}
	behaviors, err := ListBehaviors(ctx, pool, appID)
	if err != nil {
		t.Fatal(err)
	}
	if len(behaviors) != 1 || behaviors[0].Provenance.Kind != ProvenanceLearning {
		t.Fatalf("behaviors = %+v", behaviors)
	}
	if b.ID != behaviors[0].ID {
		t.Error("promoted behavior must be the listed one")
	}
	// Learning FK provenance must survive the JSONB round-trip.
	got, err := GetBehavior(ctx, pool, appID, learningBehaviorKey(l.ID))
	if err != nil || got == nil {
		t.Fatalf("re-get promoted behavior: %v %v", err, got)
	}
	if got.Provenance.LearningID != l.ID.String() || got.Provenance.RunID != runID.String() {
		t.Errorf("provenance = %+v, learning %s run %s", got.Provenance, l.ID.String(), runID.String())
	}
}

type contentsEntry = github.ContentsEntry

func writeContents(w http.ResponseWriter, entries ...contentsEntry) {
	_ = json.NewEncoder(w).Encode(entries)
}

func writeFile(w http.ResponseWriter, content string) {
	_ = json.NewEncoder(w).Encode(map[string]string{
		"name": "spec.ts", "path": "spec.ts", "type": "file", "encoding": "base64",
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	})
}
