// Package flakes owns flake-rate analytics and the test quarantine store
// (FLAK-01/02). Flake rates quantify per-test instability from real run
// history; the quarantine store is the skip signal the report stage reads —
// quarantining a test never mutates the app under test (report-only), and
// auto-retry is evidence-gated so app bugs are never retried away.
package flakes

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultWindow is the flake-rate lookback in runs when the caller passes
// <= 0: small enough to reflect the suite's current state, large enough for
// rates to mean anything (TREN-01 uses the same >= 5 floor for flip rates).
const DefaultWindow = 20

// Rate is one test's flake profile over the analysis window.
type Rate struct {
	AppID      string  `json:"app_id"`
	File       string  `json:"file"`
	Title      string  `json:"title"`
	WindowRuns int     `json:"window_runs"` // runs (in window) where the test actually executed
	FlakyRuns  int     `json:"flaky_runs"`  // runs whose report marked it outcome=flaky
	FailRuns   int     `json:"fail_runs"`   // runs whose report marked it outcome=unexpected
	PassRuns   int     `json:"pass_runs"`   // runs where it ended passed (expected or flaky)
	Rate       float64 `json:"rate"`        // FlakyRuns / WindowRuns
	Unstable   bool    `json:"unstable"`    // both a pass run and a fail run exist in the window
}

// classification constants (test_results.classification, 0001).
const (
	ClassificationAppBug   = "app_bug"
	ClassificationTestGap  = "test_gap"
	ClassificationInfra    = "infra"
	ClassificationFlakeish = "agent_flake"
)

// CanAutoRetry is the evidence-gated retry policy (FLAK-01): only
// infra-class failures may trigger an automatic retry — the suite never got
// a fair environment. An app_bug (the product is broken) or a test_gap (the
// spec is wrong) is never retried away; retrying real defects is exactly the
// red-green noise this phase exists to govern. The argument is the parsed
// classification string stored on test_results.
func CanAutoRetry(classification string) bool {
	return classification == ClassificationInfra
}

// FlakeRates computes per-test flake rates for an app over the last window
// runs that reached a verdict. Skipped rows are never execution evidence and
// are excluded at the row level; the window counts distinct executed runs,
// not rows (a retry-rich suite would otherwise inflate it).
func FlakeRates(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID, window int) ([]Rate, error) {
	if window <= 0 {
		window = DefaultWindow
	}
	rows, err := pool.Query(ctx, `
		with recent as (
			select id from runs
			where app_id = $1 and verdict is not null
			order by created_at desc
			limit $2
		)
		select
			coalesce(tr.file, ''),
			tr.title,
			count(distinct tr.run_id),
			count(distinct tr.run_id) filter (where tr.outcome = 'flaky'),
			count(distinct tr.run_id) filter (where tr.outcome = 'unexpected'),
			count(distinct tr.run_id) filter (where tr.outcome in ('expected', 'flaky') and tr.status = 'passed')
		from test_results tr
		join recent on recent.id = tr.run_id
		where tr.outcome <> 'skipped'
		group by 1, 2
	`, appID, window)
	if err != nil {
		return nil, fmt.Errorf("flakes: query rates: %w", err)
	}
	defer rows.Close()

	appIDStr := pgUUIDString(appID)
	rates := []Rate{}
	for rows.Next() {
		var r Rate
		var flaky, fails, passes int
		if err := rows.Scan(&r.File, &r.Title, &r.WindowRuns, &flaky, &fails, &passes); err != nil {
			return nil, fmt.Errorf("flakes: scan rate row: %w", err)
		}
		r.AppID = appIDStr
		r.FlakyRuns, r.FailRuns, r.PassRuns = flaky, fails, passes
		r.Rate = ratio(flaky, r.WindowRuns)
		r.Unstable = passes > 0 && fails > 0
		rates = append(rates, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("flakes: iterate rates: %w", err)
	}
	return rates, nil
}

func ratio(n, denom int) float64 {
	if denom <= 0 {
		return 0
	}
	return float64(n) / float64(denom)
}

func pgUUIDString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b, _ := id.Value()
	return b.(string)
}
