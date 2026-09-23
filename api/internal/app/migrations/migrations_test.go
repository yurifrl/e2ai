// Regression proof for the manual trigger (06-02 T2, TRIG-05): the migration
// ordering is fail-closed — the 0007 check constraint would have rejected
// trigger_source='manual', and only after 0010's drop-and-re-add does a
// manual run insert succeed. Also binds the dedicated cockpit_run_ids
// sequence (strictly increasing, so the minted negative ids are strictly
// decreasing) and the partial unique index admit/deny behavior of the
// insert-as-gate. Self-skips without E2AI_TEST_DATABASE_URL.
//
// The structural leg replays the 0007 constraint statement VERBATIM —
// extracted at runtime from the embedded migration source — against a temp
// table named `runs`, so a future edit to 0007 that widens the vocabulary
// makes this test fail loudly instead of silently validating a stale copy.
package migrations_test

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	app "github.com/NSXBet/e2ai/api/internal/app"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var sources embed.FS

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
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// pgError extracts the pgconn.PgError from an error, if any.
func pgError(err error) *pgconn.PgError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr
	}
	return nil
}

func TestManualTriggerSource(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Deterministic migration replay: the scratch DB may carry residue from
	// a prior test binary; Down+Migrate re-establishes the 0010 state.
	if err := app.Down(ctx, pool); err != nil {
		t.Fatalf("down: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := app.Migrate(ctx, pool, logger); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// --- (a) Structural proof: the pre-0010 (0007) constraint rejects manual.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	seven, err := sources.ReadFile("0007_hardening.sql")
	if err != nil {
		t.Fatalf("read embedded 0007: %v", err)
	}
	stmt, err := statementContaining(string(seven), "runs_trigger_source_check")
	if err != nil {
		t.Fatalf("extract 0007 constraint statement: %v", err)
	}

	// Temp table shadows the public `runs` for this session only, so the
	// verbatim 0007 statement applies to the fixture. Columns mirror the
	// 0001 runs DDL subset the constraint-era inserts needed.
	if _, err := conn.Exec(ctx, `
		create temp table runs (
			id uuid primary key default gen_random_uuid(),
			app_id uuid not null,
			workflow_run_id bigint not null,
			run_number int not null,
			head_sha text not null,
			head_branch text,
			environment text not null,
			trigger_source text not null default 'webhook',
			status text not null default 'queued'
		) on commit drop`); err != nil {
		t.Fatalf("temp fixture: %v", err)
	}
	if _, err := conn.Exec(ctx, stmt); err != nil {
		t.Fatalf("apply extracted 0007 constraint: %v", err)
	}

	manualInsert := func(source string) error {
		_, err := conn.Exec(ctx, `
			insert into runs (app_id, workflow_run_id, run_number, head_sha,
			                  head_branch, environment, trigger_source)
			values (gen_random_uuid(), -1, 0, 'a', null, 'staging', $1)`, source)
		return err
	}
	if err := manualInsert("manual"); err == nil {
		t.Fatal("pre-0010 constraint must reject trigger_source='manual' — fail-closed property broken")
	} else {
		pgErr := pgError(err)
		if pgErr == nil || pgErr.Code != "23514" { // check_violation
			t.Fatalf("manual insert must fail with a check violation, got: %v", err)
		}
	}
	if err := manualInsert("webhook"); err != nil {
		t.Fatalf("baseline webhook insert must succeed under the 0007 constraint: %v", err)
	}

	// --- (b) Live constraint (post-0010) admits manual + simulate.
	var def string
	if err := pool.QueryRow(ctx, `
		select pg_get_constraintdef(oid) from pg_constraint
		where conname = 'runs_trigger_source_check'`).Scan(&def); err != nil {
		t.Fatalf("live constraint lookup: %v", err)
	}
	for _, src := range []string{"manual", "simulate"} {
		if !strings.Contains(def, "'"+src+"'") {
			t.Fatalf("live constraint %q must admit %q", def, src)
		}
	}

	// --- (c) cockpit_run_ids: strictly increasing positives so the minted
	// negatives (-nextval) are strictly decreasing and always negative.
	var n1, n2 int64
	if err := pool.QueryRow(ctx,
		`select nextval('cockpit_run_ids'), nextval('cockpit_run_ids')`).Scan(&n1, &n2); err != nil {
		t.Fatalf("nextval: %v", err)
	}
	if n1 <= 0 || n2 <= n1 {
		t.Fatalf("cockpit_run_ids must advance positively (got %d, %d)", n1, n2)
	}
	if !(-n2 < -n1 && -n1 < 0) {
		t.Fatalf("minted ids must be strictly decreasing negatives: %d, %d", -n1, -n2)
	}

	// --- (d) Post-0010: a manual run inserts; the partial unique index
	// admits two apps in flight but refuses a second on the same app.
	// All inside one transaction rolled back at the end — zero residue.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	app1 := seedAppForConstraintTest(t, tx, "app-a")
	app2 := seedAppForConstraintTest(t, tx, "app-b")

	manualRun := func(tx pgx.Tx, appID string) error {
		_, err := tx.Exec(ctx, `
			insert into runs (app_id, workflow_run_id, run_number, head_sha, head_branch,
			                  environment, trigger_source, triggered_by, status)
			values ($1, -nextval('cockpit_run_ids'), 0, 'abc', '', 'staging',
			        'manual', 'test@example.com', 'queued')
			on conflict do nothing`,
			appID)
		return err
	}
	if err := manualRun(tx, app1); err != nil {
		t.Fatalf("post-0010 manual insert must succeed: %v", err)
	}
	secondTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second tx: %v", err)
	}
	if err := manualRun(secondTx, app1); err == nil {
		t.Fatal("second in-flight manual run on the same app must be refused by the partial unique index")
	} else {
		pgErr := pgError(err)
		if pgErr == nil || pgErr.Code != "23505" { // unique_violation
			t.Fatalf("same-app refusal must be a unique violation, got: %v", err)
		}
	}
	_ = secondTx.Rollback(ctx)
	if err := manualRun(tx, app2); err != nil {
		t.Fatalf("in-flight manual run on a DIFFERENT app must be admitted: %v", err)
	}
	_ = tx.Rollback(ctx)
}

// seedAppForConstraintTest inserts one minimal registered_apps row with the
// given slug as owner (unique per (owner, repo, workflow, environment)).
func seedAppForConstraintTest(t *testing.T, tx pgx.Tx, slug string) string {
	t.Helper()
	var id string
	err := tx.QueryRow(context.Background(), `
		insert into registered_apps (owner, repo, workflow_name, workflow_path,
		                             environment, display_title_pattern,
		                             webhook_secret_ref, comment_marker)
		values ($1, 'r', 'w', '.github/wf.yaml', 'staging', 'p', 'ref', '<!-- c -->')
		returning id::text`, slug).Scan(&id)
	if err != nil {
		t.Fatalf("seed app %s: %v", slug, err)
	}
	return id
}

// statementContaining extracts the first SQL statement containing needle,
// verbatim, from a migration file's text. Statements are semicolon-delimited.
func statementContaining(text, needle string) (string, error) {
	for _, stmt := range strings.Split(text, ";") {
		if strings.Contains(stmt, needle) {
			return strings.TrimSpace(stmt) + ";", nil
		}
	}
	return "", errors.New("no statement containing " + needle)
}
