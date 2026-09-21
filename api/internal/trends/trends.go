// Package trends owns the TREN-01 aggregations over real run history:
// verdict-flip rate (same app, bounded window), per-run agent cost (the
// agent_sessions accumulators with their step evidence counted), and the
// pass/fail history series. All read-only — trends never writes — except
// the alert queue's acknowledge action (TREN-01 alerting, alerts.go): an
// admin-visible alert queue without an ack action would drain nobody.
package trends

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MinWindow is the floor for flip-rate windows (TREN-01: >= 5 runs before
// a rate means anything) and the default when the caller passes less.
const MinWindow = 5

// DefaultWindow is the history lookback default.
const DefaultWindow = 20

// FlipRate is the verdict-flip profile of one app's recent suite runs.
type FlipRate struct {
	AppID      string  `json:"app_id"`
	WindowRuns int     `json:"window_runs"` // completed runs analyzed (chronological)
	Flips      int     `json:"flips"`       // consecutive verdict changes
	Rate       float64 `json:"rate"`        // Flips / max(WindowRuns-1, 1)
}

// ComputeFlipRate measures how often consecutive completed runs flip
// verdict (pass<->fail<->flaky). Only runs with a verdict count — queued
// or infra-broken runs carry no suite signal. Windows below MinWindow are
// clamped: a rate over two runs is noise, not a trend.
func ComputeFlipRate(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID, window int) (*FlipRate, error) {
	if window < MinWindow {
		window = MinWindow
	}
	rows, err := pool.Query(ctx, `
		select verdict from runs
		where app_id = $1 and verdict is not null
		order by created_at desc
		limit $2
	`, appID, window)
	if err != nil {
		return nil, fmt.Errorf("trends: query flips: %w", err)
	}
	defer rows.Close()

	// Rows arrive newest-first; reverse into chronological order so flips
	// count transitions forward in time.
	verdicts := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("trends: scan verdict: %w", err)
		}
		verdicts = append(verdicts, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trends: iterate verdicts: %w", err)
	}
	for i, j := 0, len(verdicts)-1; i < j; i, j = i+1, j-1 {
		verdicts[i], verdicts[j] = verdicts[j], verdicts[i]
	}

	flips := 0
	for i := 1; i < len(verdicts); i++ {
		if verdicts[i] != verdicts[i-1] {
			flips++
		}
	}
	fr := &FlipRate{AppID: uuidString(appID), WindowRuns: len(verdicts), Flips: flips}
	if d := len(verdicts) - 1; d > 0 {
		fr.Rate = float64(flips) / float64(d)
	}
	return fr, nil
}

// SessionCost is one agent session's resource profile within a run.
type SessionCost struct {
	SessionID      string `json:"session_id"`
	Role           string `json:"role"`
	Status         string `json:"status"`
	Steps          int    `json:"steps"`
	TokensIn       int64  `json:"tokens_in"`
	TokensOut      int64  `json:"tokens_out"`
	CostMillicents int64  `json:"cost_millicents"`
}

// RunCost is the per-run agent cost rollup: agent_sessions accumulators
// (the budget engine keeps them authoritative) with the per-session step
// count from session_steps.
type RunCost struct {
	RunID          string        `json:"run_id"`
	Sessions       []SessionCost `json:"sessions"`
	Steps          int           `json:"steps"`
	TokensIn       int64         `json:"tokens_in"`
	TokensOut      int64         `json:"tokens_out"`
	CostMillicents int64         `json:"cost_millicents"`
}

// RunCosts rolls up one run's agent cost. pgx.ErrNoRows means the run does
// not exist (the HTTP surface maps it to 404); a real run with no agent
// sessions yields an empty rollup, not an error.
func RunCosts(ctx context.Context, pool *pgxpool.Pool, runID pgtype.UUID) (*RunCost, error) {
	var exists bool
	if err := pool.QueryRow(ctx, `select exists (select 1 from runs where id = $1)`, runID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("trends: check run: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("trends: run not found: %w", errNoRows)
	}

	rows, err := pool.Query(ctx, `
		select s.id, s.role, s.status,
		       (select count(*) from session_steps st where st.session_id = s.id),
		       s.tokens_in, s.tokens_out, s.cost_millicents
		from agent_sessions s
		where s.run_id = $1
		order by s.created_at
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("trends: query session costs: %w", err)
	}
	defer rows.Close()

	rc := &RunCost{RunID: uuidString(runID), Sessions: []SessionCost{}}
	for rows.Next() {
		var sc SessionCost
		if err := rows.Scan(&sc.SessionID, &sc.Role, &sc.Status, &sc.Steps,
			&sc.TokensIn, &sc.TokensOut, &sc.CostMillicents); err != nil {
			return nil, fmt.Errorf("trends: scan session cost: %w", err)
		}
		sc.SessionID = uuidText(sc.SessionID)
		rc.Sessions = append(rc.Sessions, sc)
		rc.Steps += sc.Steps
		rc.TokensIn += sc.TokensIn
		rc.TokensOut += sc.TokensOut
		rc.CostMillicents += sc.CostMillicents
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trends: iterate session costs: %w", err)
	}
	return rc, nil
}

// HistoryPoint is one completed run in the pass/fail history series.
type HistoryPoint struct {
	RunID         string `json:"run_id"`
	Verdict       string `json:"verdict"`
	Passed        int    `json:"passed"`
	Failed        int    `json:"failed"`
	Flaky         int    `json:"flaky"`
	CreatedAt     string `json:"created_at"`
	WorkflowRunID int64  `json:"workflow_run_id"`
}

// History is the recent-verdict series for an app, oldest first, with the
// pass/fail run totals over the same window.
type History struct {
	AppID    string         `json:"app_id"`
	Window   int            `json:"window"`
	Points   []HistoryPoint `json:"points"`
	PassRuns int            `json:"pass_runs"`
	FailRuns int            `json:"fail_runs"`
}

// HistoryRecent returns the app's last window verdict-bearing runs, oldest
// first. Verdicts: pass|fail|flaky count toward PassRuns/FailRuns
// (flaky counts as a failing signal in the totals, TREN-01).
func HistoryRecent(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID, window int) (*History, error) {
	if window <= 0 {
		window = DefaultWindow
	}
	rows, err := pool.Query(ctx, `
		select id, verdict, coalesce(passed, 0), coalesce(failed, 0), coalesce(flaky, 0),
		       created_at, workflow_run_id
		from runs
		where app_id = $1 and verdict is not null
		order by created_at desc
		limit $2
	`, appID, window)
	if err != nil {
		return nil, fmt.Errorf("trends: query history: %w", err)
	}
	defer rows.Close()

	h := &History{AppID: uuidString(appID), Window: window, Points: []HistoryPoint{}}
	for rows.Next() {
		var p HistoryPoint
		var id pgtype.UUID
		var createdAt pgtype.Timestamptz
		if err := rows.Scan(&id, &p.Verdict, &p.Passed, &p.Failed, &p.Flaky,
			&createdAt, &p.WorkflowRunID); err != nil {
			return nil, fmt.Errorf("trends: scan history point: %w", err)
		}
		p.RunID = uuidString(id)
		if createdAt.Valid {
			p.CreatedAt = createdAt.Time.UTC().Format("2006-01-02T15:04:05Z")
		}
		h.Points = append(h.Points, p)
		if p.Verdict == "pass" {
			h.PassRuns++
		} else {
			h.FailRuns++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trends: iterate history: %w", err)
	}
	// newest-first query -> oldest-first series.
	for i, j := 0, len(h.Points)-1; i < j; i, j = i+1, j-1 {
		h.Points[i], h.Points[j] = h.Points[j], h.Points[i]
	}
	return h, nil
}
