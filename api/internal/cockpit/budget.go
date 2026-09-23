// Budget gate domain (BUDG-03): one query, one policy, shared by every
// actuating boundary — the manual trigger path (phase 6 plan 02) and the
// simulate path (plan 03) both call Evaluate before creating a run.
//
// The layered cap resolves per app: the app's own budget_cap_millicents when
// non-null, else the E2AI_DEFAULT_BUDGET_CAP_MILLICENTS env floor when set,
// else the explicit no-cap state (never a refusal). The distinction is typed
// because the engine's existing 0-sentinel convention
// (agent_sessions.cost_ceiling_millicents — 0 = unlimited) must NOT leak
// here: a ceiling that resolved to 0 through this layering is a refusal
// candidate, never unlimited.
package cockpit

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDefaultCapMillicents is the env floor for apps without their own cap;
// unset means uncapped.
const EnvDefaultCapMillicents = "E2AI_DEFAULT_BUDGET_CAP_MILLICENTS"

// CapKind discriminates how the effective ceiling resolved (BUDG-03): from
// the app's own column, the env floor, or the explicit no-cap state.
type CapKind string

const (
	CapApp      CapKind = "app"  // registered_apps.budget_cap_millicents
	CapEnvFloor CapKind = "env"  // E2AI_DEFAULT_BUDGET_CAP_MILLICENTS
	CapNone     CapKind = "none" // uncapped — the gate never refuses
)

// Cap is the resolved ceiling for one app.
type Cap struct {
	Kind       CapKind
	Millicents int64 // meaningful only when Capped(); 0 is a refusal candidate
}

// Capped reports whether any ceiling applies (false = the no-cap state,
// which never refuses).
func (c Cap) Capped() bool { return c.Kind != CapNone }

// Zero reports a refusal candidate: a cap that resolved to 0 — explicit or
// env floor — is never unlimited (BUDG-03), unlike the engine's 0-sentinel
// session ceilings.
func (c Cap) Zero() bool { return c.Capped() && c.Millicents == 0 }

// CapFor resolves the layered budget cap for appID (BUDG-03): the app's own
// cap when set, else the env floor when set, else the explicit no-cap state.
func CapFor(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID) (Cap, error) {
	var own *int64
	if err := pool.QueryRow(ctx,
		`select budget_cap_millicents from registered_apps where id = $1`, appID,
	).Scan(&own); err != nil {
		return Cap{}, fmt.Errorf("read budget cap: %w", err)
	}
	if own != nil {
		return Cap{Kind: CapApp, Millicents: *own}, nil
	}
	if raw := os.Getenv(EnvDefaultCapMillicents); raw != "" {
		floor, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return Cap{}, fmt.Errorf("parse %s=%q: %w", EnvDefaultCapMillicents, raw, err)
		}
		return Cap{Kind: CapEnvFloor, Millicents: floor}, nil
	}
	return Cap{Kind: CapNone}, nil
}

// DaySpend returns the app's agent-session cost total for the current UTC
// day (BUDG-03): agent_sessions.cost_millicents joined via runs, day-scoped
// on the session's own created_at. No stored per-day aggregate exists — this
// query is the spend source of truth.
func DaySpend(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID) (int64, error) {
	var spent int64
	if err := pool.QueryRow(ctx, `
		select coalesce(sum(s.cost_millicents), 0)
		from agent_sessions s
		join runs r on r.id = s.run_id
		where r.app_id = $1
		  and s.created_at >= date_trunc('day', now() at time zone 'UTC') at time zone 'UTC'
	`, appID).Scan(&spent); err != nil {
		return 0, fmt.Errorf("day spend: %w", err)
	}
	return spent, nil
}

// Verdict is the budget gate outcome for one actuation candidate (BUDG-03).
// Refused → the boundary rejects with 409 and Reason verbatim (the
// remaining-budget reason per CONTEXT decision 1).
type Verdict struct {
	Cap       Cap
	Spend     int64  // day-scoped UTC aggregate for the app
	Remaining int64  // cap - spend when capped (floored at 0); 0 when uncapped
	Refused   bool   // true → the boundary rejects with 409 + Reason
	Reason    string // verbatim 409 reason; empty when allowed
}

// Evaluate combines the layered cap with the day-scoped spend into one
// verdict: uncapped never refuses; a zero ceiling refuses (a refusal
// candidate, never unlimited); spend at or over a positive ceiling refuses
// with the remaining-budget reason; otherwise the boundary may act.
func Evaluate(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID) (Verdict, error) {
	cap, err := CapFor(ctx, pool, appID)
	if err != nil {
		return Verdict{}, err
	}
	spend, err := DaySpend(ctx, pool, appID)
	if err != nil {
		return Verdict{}, err
	}
	v := Verdict{Cap: cap, Spend: spend}
	if !cap.Capped() {
		return v, nil
	}
	v.Remaining = cap.Millicents - spend
	if v.Remaining < 0 {
		v.Remaining = 0
	}
	switch {
	case cap.Zero():
		v.Refused = true
		v.Reason = "budget ceiling is zero — no spend is permitted for this app today"
	case spend >= cap.Millicents:
		v.Refused = true
		v.Reason = fmt.Sprintf("daily budget exhausted: spent %d of %d millicents today, %d remaining",
			spend, cap.Millicents, v.Remaining)
	}
	return v, nil
}
