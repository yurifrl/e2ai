// Sync lifecycle tests (PLAN-02, APPR-01) run against a real scratch Postgres
// (E2AI_TEST_DATABASE_URL — ephemeral docker postgres:16-alpine) so content
// hashing, draft-vs-pending branching, and the approved-ancestor lookup hit
// the actual 0002_plans constraints instead of mock rows.
package plans

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"os"
	"testing"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/platform/github"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const validPlan = `---
id: checkout-flow-v1.0.0
app: NSXBet/bet-app
title: Checkout flow stays green
surfaces: [web]
trigger: on-deploy
sources: []
---

## Steps

1. Open the checkout page as a logged-in user.
   judge: the balance widget is visible
2. Place a bet with a stake of 10.
   judge: the potential win shows at least 20
   uses: MATCH-LIVE-BADGE
`

// changedPlan keeps id/app/title but rewrites step 2's judge: ContentHash
// moves while plan identity stays put — the approval-branch case.
const changedPlan = `---
id: checkout-flow-v1.0.0
app: NSXBet/bet-app
title: Checkout flow stays green
surfaces: [web]
trigger: on-deploy
sources: []
---

## Steps

1. Open the checkout page as a logged-in user.
   judge: the balance widget is visible
2. Place a bet with a stake of 25.
   judge: the potential win shows at least 50
   uses: MATCH-LIVE-BADGE
`

func plansTestPool(t *testing.T) *pgxpool.Pool {
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
	for _, table := range []string{"plans", "plan_versions", "plan_approvals", "corpus_behaviors", "corpus_learnings", "registered_apps", "river_job"} {
		if _, err := pool.Exec(ctx, "truncate table "+table+" cascade"); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	return pool
}

// plansTestApp seeds one registered_apps row whose plans_source declares the
// repo_path directory, and returns its id.
func plansTestApp(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment, display_title_pattern,
			 webhook_secret_ref, comment_marker, plans_source)
		values ('NSXBet', 'bet-app', 'Release - Web', '.github/workflows/remote_workflow_betweb.yaml',
			'staging', '\[BetWeb\] Deploy to staging', 'op://x/secret', '<!-- e2ai:run:bet-app-web -->',
			$1)
		returning id`,
		"kind: repo_path\npath: e2ai/plans").Scan(&id)
	if err != nil {
		t.Fatalf("seed registered_app fixture: %v", err)
	}
	return id
}

// seedVerifiedBehavior inserts the corpus behavior a plan's `uses:` must
// resolve to (validate.go requires status verified).
func seedVerifiedBehavior(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		insert into corpus_behaviors (app_id, key, title, description, status, provenance)
		values ($1, 'MATCH-LIVE-BADGE', 'MATCH-LIVE-BADGE', 'seeded by plans sync tests', 'verified',
			'{"kind":"playwright-spec","path":"e2e/live.spec.ts"}'::jsonb)
		on conflict (app_id, key) do update set status = 'verified'`, appID)
	if err != nil {
		t.Fatalf("seed verified behavior: %v", err)
	}
}

// fakePlansRepo stands up a fake GitHub contents API serving the given files
// under e2ai/plans/ and returns a client pointed at it.
func fakePlansRepo(t *testing.T, files map[string]string) *github.Client {
	t.Helper()
	mux := http.NewServeMux()
	refs := map[string]bool{"main": true, "HEAD": true, "deadbeef": true}
	// The directory listing walkPlans starts from: one entry per seeded file.
	entries := "["
	first := true
	for path := range files {
		if !first {
			entries += ","
		}
		first = false
		entries += fmt.Sprintf(`{"type":"file","path":%q,"name":%q}`, path, filepath.Base(path))
	}
	entries += "]"
	dirHandler := func(w http.ResponseWriter, r *http.Request) {
		if !refs[r.URL.Query().Get("ref")] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(entries))
	}
	// Handler patterns must carry the same %2F escaping the github client
	// sends (url.PathEscape) — ServeMux matches escaped segments distinctly.
	mux.HandleFunc("GET /repos/NSXBet/bet-app/contents/"+url.PathEscape("e2ai/plans"), dirHandler)
	for path, content := range files {
		p := "/repos/NSXBet/bet-app/contents/" + url.PathEscape(path)
		c := content
		mux.HandleFunc("GET "+p, func(w http.ResponseWriter, r *http.Request) {
			if !refs[r.URL.Query().Get("ref")] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"type":"file","content":%q,"encoding":"base64"}`,
				base64.StdEncoding.EncodeToString([]byte(c)))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := github.NewWithBaseURL("t", srv.URL)
	if err != nil {
		t.Fatalf("github client: %v", err)
	}
	return client
}

func versionState(t *testing.T, pool *pgxpool.Pool, planID string) (status, contentHash string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`select status, content_hash from plan_versions where plan_id = $1 limit 1`, planID).
		Scan(&status, &contentHash)
	if err != nil {
		t.Fatalf("version state for %s: %v", planID, err)
	}
	return status, contentHash
}

// The lifecycle core (APPR-01): a never-approved plan's first sync lands as
// draft; identical re-sync is a no-op.
func TestSyncAppDraftLifecycle(t *testing.T) {
	pool := plansTestPool(t)
	ctx := context.Background()
	appID := plansTestApp(t, pool)
	seedVerifiedBehavior(t, pool, appID)

	client := fakePlansRepo(t, map[string]string{"checkout.md": validPlan})

	res, err := SyncAppByID(ctx, pool, client, appID)
	if err != nil {
		t.Fatalf("SyncAppByID: %v", err)
	}
	if res.Synced != 1 || res.Added != 1 {
		t.Fatalf("result = %+v, want Synced=1 Added=1", res)
	}
	status, hash := versionState(t, pool, "checkout-flow-v1.0.0")
	if status != "draft" {
		t.Fatalf("first sync status = %q, want draft (never-approved plan must not execute)", status)
	}
	if hash == "" {
		t.Fatal("content_hash empty — identity not populated")
	}

	// Content-hash identity: re-sync of the same bytes is a no-op.
	res, err = SyncAppByID(ctx, pool, client, appID)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if res.Unchanged != 1 {
		t.Errorf("re-sync result = %+v, want Unchanged=1", res)
	}
}

// Approval branching: content change on an approved plan lands
// pending_approval while the approved version stays intact — the run-time
// gate keeps executing the approved content until a human re-approves.
func TestSyncAppApprovalBranching(t *testing.T) {
	pool := plansTestPool(t)
	ctx := context.Background()
	appID := plansTestApp(t, pool)
	seedVerifiedBehavior(t, pool, appID)

	client := fakePlansRepo(t, map[string]string{"checkout.md": validPlan})
	if _, err := SyncAppByID(ctx, pool, client, appID); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Approve the draft (approval recording is T6; insert directly here).
	if _, err := pool.Exec(ctx, `
		insert into plan_approvals (plan_version_id, decision, approver, notes)
		select id, 'approved', 'yuri@example.com', 'test approval'
		from plan_versions where plan_id = 'checkout-flow-v1.0.0'`); err != nil {
		t.Fatalf("record approval: %v", err)
	}

	// Changed content: same plan identity, different judge text.
	changedClient := fakePlansRepo(t, map[string]string{"checkout.md": changedPlan})
	res, err := SyncAppByID(ctx, pool, changedClient, appID)
	if err != nil {
		t.Fatalf("changed sync: %v", err)
	}
	if res.Changed != 1 {
		t.Errorf("changed sync result = %+v, want Changed=1", res)
	}

	// The approved ancestor's content stays pinned by its approval record —
	// T6's real flow flips version status; sync only ever ADDS versions.
	var approvedHash, decision string
	if err := pool.QueryRow(ctx, `
		select pv.content_hash, pa.decision
		from plan_versions pv join plan_approvals pa on pa.plan_version_id = pv.id
		where pv.plan_id = $1`,
		"checkout-flow-v1.0.0").Scan(&approvedHash, &decision); err != nil {
		t.Fatalf("approved ancestor vanished after change: %v", err)
	}
	if decision != "approved" {
		t.Fatalf("approval record = %q, want approved", decision)
	}
	var pendingHash string
	if err := pool.QueryRow(ctx,
		`select content_hash from plan_versions where plan_id = $1 and status = 'pending_approval'`,
		"checkout-flow-v1.0.0").Scan(&pendingHash); err != nil {
		t.Fatalf("pending_approval version missing after change: %v", err)
	}
	if approvedHash == pendingHash {
		t.Fatal("approved and pending share a content hash — identity broken")
	}
}

// An app without plans_source is skipped loudly by the worker path and is an
// error from the direct path.
func TestSyncAppNoPlansSource(t *testing.T) {
	pool := plansTestPool(t)
	ctx := context.Background()

	var id pgtype.UUID
	err := pool.QueryRow(ctx, `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment, display_title_pattern,
			 webhook_secret_ref, comment_marker)
		values ('NSXBet', 'other-app', 'Release - Web', '.github/workflows/w.yaml', 'staging',
			'\[Other\]', 'op://x/secret', '<!-- e2ai:run:other -->')
		returning id`).Scan(&id)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}

	_, err = SyncAppByID(ctx, pool, nil, id)
	if err == nil {
		t.Fatal("SyncAppByID without plans_source must fail loudly")
	}
}
