// The draft-seam lifecycle test (DERV-02, APPR-01) runs against the scratch
// Postgres (E2AI_TEST_DATABASE_URL) so the load-bearing property — a
// derivation draft lands status draft even when the parent plan already has
// an approved version — is verified against the real 0002_plans state
// machine instead of mocked rows.
package derive

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/corpus"
	"github.com/NSXBet/e2ai/api/internal/plans"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testDerivePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("E2AI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("E2AI_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dial scratch postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := app.Migrate(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("migrate schema: %v", err)
	}
	if err := app.MigrateRiver(ctx, pool); err != nil {
		t.Fatalf("migrate river: %v", err)
	}
	for _, table := range []string{"run_plan_resolutions", "plans", "plan_versions", "plan_approvals", "corpus_behaviors", "corpus_learnings", "registered_apps", "river_job"} {
		if _, err := pool.Exec(ctx, "truncate table "+table+" cascade"); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	return pool
}

func seedDraftApp(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(context.Background(), `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment, display_title_pattern,
			 webhook_secret_ref, comment_marker, surfaces)
		values ('NSXBet', 'bet-app', 'Release - Web', '.github/workflows/remote_workflow_betweb.yaml',
			'staging', '\[BetWeb\] Deploy to staging', 'op://x/secret', '<!-- e2ai:run:bet-app-web -->', $1)
		returning id`,
		[]string{"web", "mobile"}).Scan(&id); err != nil {
		t.Fatalf("seed registered_app: %v", err)
	}
	return id
}

func seedVerifiedBehavior(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID, key string) {
	t.Helper()
	b := corpus.Behavior{
		AppID: appID, Key: key,
		Title:       key + " renders",
		Description: "the live odds badge is visible on the match page",
		Status:      corpus.BehaviorVerified,
		Provenance:  corpus.Provenance{Kind: corpus.ProvenancePlaywrightSpec, Path: "web/tests/live-badge.spec.ts"},
		VerifiedBy:  pgtype.Text{String: "tester", Valid: true},
	}
	if err := corpus.UpsertBehavior(context.Background(), pool, &b); err != nil {
		t.Fatalf("seed behavior %s: %v", key, err)
	}
}

// TestDraftStoresAsDraftNotPending: the parent plan is approved, the derive
// seam stores a new version over it, and the stored status must still be
// draft — pending_approval is human-only, even with an approved ancestor.
func TestDraftStoresAsDraftNotPending(t *testing.T) {
	pool := testDerivePool(t)
	appID := seedDraftApp(t, pool)
	ctx := context.Background()

	// Parent plan: approved version.
	parent := `---
id: checkout-flow-v1.0.0
app: NSXBet/bet-app
title: Checkout flow
surfaces: [web]
trigger: on-demand
---
1. Open the page.
   judge: the badge is visible
`
	if _, err := plans.UpsertDraftVersion(ctx, pool, appID, "e2ai/plans/checkout.md", parent); err != nil {
		t.Fatalf("seed parent draft: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into plan_approvals (plan_version_id, decision, approver)
		select id, 'approved', 'tester' from plan_versions
		where plan_id = 'checkout-flow-v1.0.0'`); err != nil {
		t.Fatalf("approve parent: %v", err)
	}

	// Derivation draft over the approved parent.
	seedVerifiedBehavior(t, pool, appID, "PWSPEC-LIVE-BADGE")
	draftID := BumpVersion("checkout-flow-v1.0.0")
	content, err := Render(DraftSpec{
		ParentPlanID: "checkout-flow-v1.0.0",
		AppRef:       "NSXBet/bet-app",
		Title:        "Derived: checkout flow",
		Surfaces:     []string{"web"},
		Trigger:      "on-deploy",
		Behaviors: []corpus.Behavior{{
			Key:         "PWSPEC-LIVE-BADGE",
			Description: "the live odds badge is visible on the match page",
		}},
	})
	if err != nil {
		t.Fatalf("render draft: %v", err)
	}

	outcome, err := plans.UpsertDraftVersion(ctx, pool, appID, DraftPath(draftID), content)
	if err != nil {
		t.Fatalf("store draft: %v", err)
	}
	if outcome != "added" {
		t.Fatalf("outcome = %q, want added", outcome)
	}

	var status, createdBy string
	if err := pool.QueryRow(ctx,
		`select status, created_by from plan_versions where plan_id = $1`, draftID).
		Scan(&status, &createdBy); err != nil {
		t.Fatalf("read draft version: %v", err)
	}
	if status != "draft" {
		t.Fatalf("draft stored with status %q; derivation must never land pending_approval without a human", status)
	}
	if createdBy != "derive-draft" {
		t.Fatalf("created_by = %q, want derive-draft", createdBy)
	}

	// The tautology guard holds end-to-end: sources carry corpus provenance.
	var sources []string
	if err := pool.QueryRow(ctx,
		`select sources from plan_versions where plan_id = $1`, draftID).Scan(&sources); err != nil {
		t.Fatalf("read sources: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("draft stored with empty sources")
	}
	for _, id := range sources {
		if plans.IsDiffSource(id) {
			t.Fatalf("source %q is diff-derived: tautology guard violated", id)
		}
	}

	// The parent's approval record is untouched by the draft.
	var approvedHash, decision string
	if err := pool.QueryRow(ctx, `
		select pv.content_hash, pa.decision
		from plan_approvals pa join plan_versions pv on pv.id = pa.plan_version_id
		where pv.plan_id = 'checkout-flow-v1.0.0'`).
		Scan(&approvedHash, &decision); err != nil {
		t.Fatalf("approved ancestor vanished after draft: %v", err)
	}
	if decision != "approved" {
		t.Fatalf("approval record = %q, want approved", decision)
	}
}
