// Store tests run against a real scratch Postgres (E2AI_TEST_DATABASE_URL —
// ephemeral docker postgres:16-alpine) so the DDL check constraints, JSONB
// provenance round-trip, and unique (app_id, key) idempotency are all
// exercised, not just the query text.
package corpus

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("E2AI_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("E2AI_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("dial test database: %v", err)
	}
	t.Cleanup(pool.Close)

	// Corpus tables are 0002_plans.sql — bring the schema up before use.
	if err := app.Migrate(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("migrate schema: %v", err)
	}
	// Tests assume a clean slate: repeated runs against one scratch database
	// must not see fixtures from earlier runs.
	for _, table := range []string{"corpus_learnings", "corpus_behaviors", "runs", "registered_apps"} {
		if _, err := pool.Exec(ctx, "truncate table "+table+" cascade"); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	return pool
}

var testAppSeq atomic.Int64

// testApp inserts one registered_apps row (FK root for corpus tables) and
// returns its id.
func testApp(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	seq := testAppSeq.Add(1)
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment, display_title_pattern, webhook_secret_ref, comment_marker)
		values ($1, 'bet-app', 'Release - Web', '.github/workflows/remote_workflow.yml', 'staging',
			'[BetWeb] Deploy to staging', 'op://x/secret', '<!-- e2ai:run:test -->')
		returning id`,
		fmt.Sprintf("e2ai-test-%d", seq)).Scan(&id)
	if err != nil {
		t.Fatalf("seed registered_app fixture: %v", err)
	}
	return id
}

// testRun inserts one runs row for app and returns its id (learning FK).
func testRun(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into runs (app_id, workflow_run_id, run_number, environment, head_sha)
		values ($1, $2, 1, 'staging', 'deadbeef') returning id`,
		appID, 9000+testAppSeq.Load()).Scan(&id)
	if err != nil {
		t.Fatalf("seed run fixture: %v", err)
	}
	return id
}

func TestUpsertBehaviorIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	appID := testApp(t, pool)

	b := Behavior{
		AppID: appID, Key: "PWSPEC-LIVE-BADGE",
		Title:       "live odds > shows the live badge",
		Description: "live odds > shows the live badge — expects: expect(badge).toBeVisible()",
		Status:      BehaviorVerified,
		Provenance:  Provenance{Kind: ProvenancePlaywrightSpec, Path: "e2e/live.spec.ts", Commit: "abc123"},
	}
	if err := UpsertBehavior(ctx, pool, &b); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := UpsertBehavior(ctx, pool, &b); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `select count(*) from corpus_behaviors where app_id = $1`, appID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("re-seed must not duplicate rows, got %d", count)
	}

	got, err := GetBehavior(ctx, pool, appID, "PWSPEC-LIVE-BADGE")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("behavior missing after upsert")
	}
	if got.Provenance != b.Provenance || got.Title != b.Title || got.Description != b.Description {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got.Provenance, b.Provenance)
	}
	if got.Status != BehaviorVerified {
		t.Errorf("status = %q, want verified", got.Status)
	}

	// Changed spec content refreshes the row, not a second one.
	b2 := b
	b2.Description = "updated description"
	b2.Provenance.Commit = "def456"
	if err := UpsertBehavior(ctx, pool, &b2); err != nil {
		t.Fatalf("upsert changed: %v", err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from corpus_behaviors where app_id = $1`, appID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("changed seed must refresh in place, got %d rows", count)
	}
}

func TestUpsertBehaviorRejectsInvalid(t *testing.T) {
	pool := testPool(t)
	appID := testApp(t, pool)
	cases := map[string]Behavior{
		"no app":     {Key: "K", Title: "t", Description: "d", Status: BehaviorVerified, Provenance: Provenance{Kind: "playwright-spec"}},
		"no key":     {AppID: appID, Title: "t", Description: "d", Status: BehaviorVerified, Provenance: Provenance{Kind: "playwright-spec"}},
		"no status":  {AppID: appID, Key: "K", Title: "t", Description: "d", Provenance: Provenance{Kind: "playwright-spec"}},
		"no source":  {AppID: appID, Key: "K", Title: "t", Description: "d", Status: BehaviorVerified},
		"no kind":    {AppID: appID, Key: "K", Title: "t", Description: "d", Status: BehaviorVerified, Provenance: Provenance{Path: "x"}},
		"nil struct": {AppID: appID},
	}
	for name, b := range cases {
		b := b
		t.Run(name, func(t *testing.T) {
			if err := UpsertBehavior(context.Background(), pool, &b); err == nil {
				t.Errorf("%s: expected rejection", name)
			}
		})
	}
}

func TestAppendListPromoteDismissLearning(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	appID := testApp(t, pool)
	runID := testRun(t, pool, appID)

	// Append-only: observations land unverified, in order, unchanged.
	l1, err := AppendLearning(ctx, pool, appID, runID, "the odds grid flashes empty during rollover")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	l2, err := AppendLearning(ctx, pool, appID, pgtype.UUID{}, "sticky header overlaps first row on mobile")
	if err != nil {
		t.Fatalf("append (no run): %v", err)
	}
	if _, err := AppendLearning(ctx, pool, appID, runID, "   "); err != ErrEmptyObservation {
		t.Errorf("blank observation: want ErrEmptyObservation, got %v", err)
	}

	learnings, err := ListLearnings(ctx, pool, appID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(learnings) != 2 {
		t.Fatalf("want 2 learnings, got %d", len(learnings))
	}
	if learnings[0].ID != l2.ID {
		t.Error("list must be newest first")
	}
	if learnings[0].RunID.Valid {
		t.Error("learning without run must have null run_id")
	}
	if !learnings[1].RunID.Valid {
		t.Error("learning with run must pin its run_id")
	}
	if learnings[0].Status != LearningUnverified || learnings[1].Status != LearningUnverified {
		t.Error("fresh learnings must be unverified")
	}

	// Promote: behavior copy carries provenance.kind=learning + verified_by.
	promoted, err := PromoteLearning(ctx, pool, l1.ID, "yuri")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted.Status != BehaviorVerified || promoted.VerifiedBy.String != "yuri" {
		t.Errorf("promoted behavior status/by = %q/%q, want verified/yuri", promoted.Status, promoted.VerifiedBy.String)
	}
	if promoted.Provenance.Kind != ProvenanceLearning || promoted.Provenance.LearningID != l1.ID.String() {
		t.Errorf("promote provenance = %+v, want kind=learning learning_id=%s", promoted.Provenance, l1.ID.String())
	}
	if promoted.Provenance.RunID != runID.String() {
		t.Errorf("promote must pin the originating run, got run_id=%q", promoted.Provenance.RunID)
	}
	if promoted.Description != l1.Observation {
		t.Errorf("description must copy the observation verbatim, got %q", promoted.Description)
	}
	if promoted.Key != learningBehaviorKey(l1.ID) {
		t.Errorf("behavior key = %q, want %q", promoted.Key, learningBehaviorKey(l1.ID))
	}

	// Learning flipped to promoted; the observation row is untouched.
	learnings, err = ListLearnings(ctx, pool, appID)
	if err != nil {
		t.Fatal(err)
	}
	if learnings[1].Status != LearningPromoted || learnings[1].Observation != l1.Observation {
		t.Errorf("promoted learning row mutated: %+v", learnings[1])
	}

	// One-shot decision: re-promote and post-dismiss promote both refused.
	if _, err := PromoteLearning(ctx, pool, l1.ID, "yuri"); err != ErrAlreadyPromoted {
		t.Errorf("re-promote: want ErrAlreadyPromoted, got %v", err)
	}
	if err := DismissLearning(ctx, pool, l1.ID); err != ErrAlreadyPromoted {
		t.Errorf("dismiss promoted: want ErrAlreadyPromoted, got %v", err)
	}
	if err := DismissLearning(ctx, pool, l2.ID); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if err := DismissLearning(ctx, pool, l2.ID); err != ErrAlreadyDismissed {
		t.Errorf("re-dismiss: want ErrAlreadyDismissed, got %v", err)
	}
	if _, err := PromoteLearning(ctx, pool, l2.ID, "yuri"); err != ErrAlreadyDismissed {
		t.Errorf("promote dismissed: want ErrAlreadyDismissed, got %v", err)
	}

	// Unattributed promotion is impossible (CORP-02 human-only writes).
	l3, err := AppendLearning(ctx, pool, appID, runID, "third observation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PromoteLearning(ctx, pool, l3.ID, "  "); err == nil {
		t.Error("promote without attributed user must fail")
	}

	// Unknown ids surface as ErrNotFound, not silent no-ops.
	var missing pgtype.UUID
	if _, err := PromoteLearning(ctx, pool, missing, "yuri"); err != ErrNotFound {
		t.Errorf("promote missing: want ErrNotFound, got %v", err)
	}
	if err := DismissLearning(ctx, pool, missing); err != ErrNotFound {
		t.Errorf("dismiss missing: want ErrNotFound, got %v", err)
	}
}
