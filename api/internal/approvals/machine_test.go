// State-machine tests (APPR-01): the pure transition table, plus the
// transactional Decide/Submit paths against the scratch Postgres so the
// conditional status flip, superseding, and the plan-level status refresh
// hit the real 0002 constraints.
package approvals

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTransition(t *testing.T) {
	cases := []struct {
		from     string
		decision string
		want     string
		wantErr  error
	}{
		{StatusDraft, "submit", StatusPendingApproval, nil},
		{StatusPendingApproval, "approved", StatusApproved, nil},
		{StatusPendingApproval, "rejected", StatusRejected, nil},
		{StatusDraft, "approved", "", ErrNotPending},
		{StatusDraft, "rejected", "", ErrNotPending},
		{StatusApproved, "approved", "", ErrNotPending},
		{StatusApproved, "rejected", "", ErrNotPending},
		{StatusRejected, "approved", "", ErrNotPending},
		{StatusSuperseded, "approved", "", ErrNotPending},
		{StatusApproved, "submit", "", ErrNotDraft},
		{StatusPendingApproval, "submit", "", ErrNotDraft},
		{StatusDraft, "fire", "", ErrBadDecision},
	}
	for _, tc := range cases {
		got, err := Transition(tc.from, tc.decision)
		if tc.wantErr == nil && err != nil {
			t.Errorf("Transition(%q, %q) err = %v, want nil", tc.from, tc.decision, err)
			continue
		}
		if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
			t.Errorf("Transition(%q, %q) err = %v, want %v", tc.from, tc.decision, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("Transition(%q, %q) = %q, want %q", tc.from, tc.decision, got, tc.want)
		}
	}
}

func testPool(t *testing.T) *pgxpool.Pool {
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

func seedTestApp(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment, display_title_pattern,
			 webhook_secret_ref, comment_marker)
		values ('NSXBet', 'bet-app', 'Release - Web', '.github/workflows/remote_workflow_betweb.yaml',
			'staging', '\[BetWeb\] Deploy to staging', 'op://x/secret', '<!-- e2ai:run:bet-app-web -->')
		returning id`).Scan(&id)
	if err != nil {
		t.Fatalf("seed registered_app fixture: %v", err)
	}
	return id
}

func seedPlan(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID, planID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		insert into plans (id, app_id, source_path, status)
		values ($1, $2, $3, 'draft')`,
		planID, appID, "e2ai/plans/"+planID+".md")
	if err != nil {
		t.Fatalf("seed plan %s: %v", planID, err)
	}
}

func seedVersion(t *testing.T, pool *pgxpool.Pool, planID, version, status string) pgtype.UUID {
	t.Helper()
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(planID+"@"+version)))
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into plan_versions (plan_id, version, content_hash, content, frontmatter, status, sources, created_by)
		values ($1, $2, $3, $4, $5::jsonb, $6, '[]'::jsonb, 'sync') returning id`,
		planID, version, hash,
		"# "+planID+"\n1. open the checkout\n   judge: the checkout completes\n",
		fmt.Sprintf(`{"id":"%s-v%s","app":"NSXBet/bet-app","title":"Checkout","surfaces":["web"],"trigger":"on-deploy"}`,
			planID, version),
		status).Scan(&id)
	if err != nil {
		t.Fatalf("seed version %s@%s: %v", planID, version, err)
	}
	return id
}

func versionStatus(t *testing.T, pool *pgxpool.Pool, vid pgtype.UUID) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`select status from plan_versions where id = $1`, vid).Scan(&status); err != nil {
		t.Fatalf("read version status: %v", err)
	}
	return status
}

func planStatus(t *testing.T, pool *pgxpool.Pool, planID string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`select status from plans where id = $1`, planID).Scan(&status); err != nil {
		t.Fatalf("read plan status: %v", err)
	}
	return status
}

func TestSubmitLifecycle(t *testing.T) {
	pool := testPool(t)
	appID := seedTestApp(t, pool)
	seedPlan(t, pool, appID, "checkout-flow")
	vid := seedVersion(t, pool, "checkout-flow", "1.0.0", StatusDraft)

	if err := Submit(context.Background(), pool, vid); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := versionStatus(t, pool, vid); got != StatusPendingApproval {
		t.Errorf("status = %q, want pending_approval", got)
	}
	if got := planStatus(t, pool, "checkout-flow"); got != "pending" {
		t.Errorf("plan status = %q, want pending", got)
	}
	if err := Submit(context.Background(), pool, vid); !errors.Is(err, ErrNotDraft) {
		t.Errorf("re-submit err = %v, want ErrNotDraft", err)
	}
	var missing pgtype.UUID
	err := Submit(context.Background(), pool, missing)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown version err = %v, want ErrNotFound", err)
	}
}

func TestDecideApproveSupersedesPrevious(t *testing.T) {
	pool := testPool(t)
	appID := seedTestApp(t, pool)
	seedPlan(t, pool, appID, "checkout-flow")
	v1 := seedVersion(t, pool, "checkout-flow", "1.0.0", StatusApproved)
	v2 := seedVersion(t, pool, "checkout-flow", "1.1.0", StatusPendingApproval)

	a, err := Decide(context.Background(), pool, v2, "approved", "alice", "looks good")
	if err != nil {
		t.Fatalf("decide approve: %v", err)
	}
	if a.Decision != "approved" || a.Approver != "alice" || a.PlanID != "checkout-flow" {
		t.Errorf("approval = %+v", a)
	}
	if a.Notes == nil || *a.Notes != "looks good" {
		t.Errorf("notes = %v, want 'looks good'", a.Notes)
	}
	if got := versionStatus(t, pool, v2); got != StatusApproved {
		t.Errorf("v2 status = %q, want approved", got)
	}
	if got := versionStatus(t, pool, v1); got != StatusSuperseded {
		t.Errorf("v1 status = %q, want superseded", got)
	}
	if got := planStatus(t, pool, "checkout-flow"); got != "approved" {
		t.Errorf("plan status = %q, want approved", got)
	}
}

func TestDecideReject(t *testing.T) {
	pool := testPool(t)
	appID := seedTestApp(t, pool)
	seedPlan(t, pool, appID, "checkout-flow")
	vid := seedVersion(t, pool, "checkout-flow", "1.0.0", StatusPendingApproval)

	a, err := Decide(context.Background(), pool, vid, "rejected", "bob", "")
	if err != nil {
		t.Fatalf("decide reject: %v", err)
	}
	if a.Notes != nil {
		t.Errorf("notes = %v, want null for empty", a.Notes)
	}
	if got := versionStatus(t, pool, vid); got != StatusRejected {
		t.Errorf("version status = %q, want rejected", got)
	}
	if got := planStatus(t, pool, "checkout-flow"); got != "rejected" {
		t.Errorf("plan status = %q, want rejected", got)
	}
}

func TestDecideConflicts(t *testing.T) {
	pool := testPool(t)
	appID := seedTestApp(t, pool)
	seedPlan(t, pool, appID, "checkout-flow")
	draft := seedVersion(t, pool, "checkout-flow", "0.9.0", StatusDraft)
	decided := seedVersion(t, pool, "checkout-flow", "1.0.0", StatusApproved)

	if _, err := Decide(context.Background(), pool, draft, "approved", "alice", ""); !errors.Is(err, ErrNotPending) {
		t.Errorf("decide draft err = %v, want ErrNotPending", err)
	}
	if _, err := Decide(context.Background(), pool, decided, "rejected", "alice", ""); !errors.Is(err, ErrNotPending) {
		t.Errorf("re-decide approved err = %v, want ErrNotPending", err)
	}
	var missing pgtype.UUID
	if _, err := Decide(context.Background(), pool, missing, "approved", "alice", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown version err = %v, want ErrNotFound", err)
	}
	if _, err := Decide(context.Background(), pool, decided, "fire", "alice", ""); !errors.Is(err, ErrBadDecision) {
		t.Errorf("bad decision err = %v, want ErrBadDecision", err)
	}
}
