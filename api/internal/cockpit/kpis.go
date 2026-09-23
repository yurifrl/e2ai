// Command Center KPI header (COCK-03): four aggregates over the same
// locked sources as the feed, scoped to the current UTC day. The budget
// card reuses the gate's own cap resolution (CapFor) verbatim — the
// cockpit never invents a parallel spend or cap calculation. Uncapped apps
// surface as a count rather than a fabricated cap. Pending approvals
// counts the approvals machine's pending_approval state
// (approvals/machine.go) — the queue is global by design (plan_versions
// carry no app identity), so the KPI stays unfiltered even when the header
// narrows to one app.
package cockpit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/NSXBet/e2ai/api/internal/approvals"
	"github.com/jackc/pgx/v5/pgtype"
)

// BudgetHeader is the arm-wide budget card: today's spend against the sum
// of resolved caps, with uncapped apps surfaced as a count instead of a
// fabricated ceiling.
type BudgetHeader struct {
	SpentToday    int64 `json:"spent_today"`
	CapsSum       int64 `json:"caps_sum"`
	UncappedCount int   `json:"uncapped_count"`
}

// KPIs is the header row (wire contract locked by the plan).
type KPIs struct {
	RunsToday        int          `json:"runs_today"`
	PassRateToday    float64      `json:"pass_rate_today"`
	Budget           BudgetHeader `json:"budget"`
	PendingApprovals int          `json:"pending_approvals"`
}

// KPIs loads the aggregates for the current UTC day, optionally narrowed
// to one app.
func (s *Service) KPIs(ctx context.Context, appID string) (KPIs, error) {
	if s.Pool == nil {
		return KPIs{}, errors.New("postgres unavailable")
	}

	// UTC-day window: the same date_trunc('day', now() at time zone 'UTC')
	// scope DaySpend uses, materialized in Go for the runs/sessions
	// predicates so all aggregates share one boundary.
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	args := []any{start, now}
	where := ""
	if appID != "" {
		id, err := parseAppID(appID)
		if err != nil {
			return KPIs{}, err
		}
		args = append(args, id)
		where = fmt.Sprintf(" and r.app_id = $%d", len(args))
	}

	// Runs today, pass rate over completed runs, manual share (the trigger
	// column operators steer by). Pass rate divides completed rows — a
	// finished-but-unreported run is transient (the report step writes the
	// verdict milliseconds after completion), so completed-with-verdict
	// numerator over completed denominator renders 0/0 honestly as 0
	// before the first report lands.
	var k KPIs
	err := s.Pool.QueryRow(ctx, `
		select
			count(*)::int,
			coalesce(
				count(*) filter (where r.status = 'completed' and r.verdict = 'pass')::float
				/ nullif(count(*) filter (where r.status = 'completed')::float, 0),
				0)
		from runs r
		where r.created_at >= $1 and r.created_at < $2`+where,
		args...,
	).Scan(&k.RunsToday, &k.PassRateToday)
	if err != nil {
		return KPIs{}, fmt.Errorf("kpi runs: %w", err)
	}

	// Spend today from the same session-cost source DaySpend aggregates.
	err = s.Pool.QueryRow(ctx, `
		select coalesce(sum(se.cost_millicents), 0)
		from agent_sessions se
		join runs r on r.id = se.run_id
		where se.created_at >= $1 and se.created_at < $2`+where,
		args...,
	).Scan(&k.Budget.SpentToday)
	if err != nil {
		return KPIs{}, fmt.Errorf("kpi spend: %w", err)
	}

	// Approvals queue depth: global by design (plan_versions carry no app
	// identity — the pending list in approvals/queries.go is unfiltered).
	err = s.Pool.QueryRow(ctx, `
		select count(*)::int from plan_versions where status = $1`,
		approvals.StatusPendingApproval,
	).Scan(&k.PendingApprovals)
	if err != nil {
		return KPIs{}, fmt.Errorf("kpi pending approvals: %w", err)
	}

	// Caps resolve through CapFor per app — the same layered resolution the
	// gate uses (app cap, then env floor, then the explicit no-cap state),
	// never a parallel SQL re-implementation. Registry-scale N+1.
	caps, uncapped, err := s.capsSum(ctx)
	if err != nil {
		return KPIs{}, err
	}
	k.Budget.CapsSum = caps
	k.Budget.UncappedCount = uncapped
	return k, nil
}

// capsSum walks registered apps through CapFor, summing the capped
// ceilings and counting the explicit no-cap states.
func (s *Service) capsSum(ctx context.Context) (int64, int, error) {
	rows, err := s.Pool.Query(ctx, `select id from registered_apps`)
	if err != nil {
		return 0, 0, fmt.Errorf("kpi caps: %w", err)
	}
	defer rows.Close()
	var sum int64
	uncapped := 0
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			return 0, 0, fmt.Errorf("kpi caps scan: %w", err)
		}
		cap, err := CapFor(ctx, s.Pool, id)
		if err != nil {
			return 0, 0, fmt.Errorf("kpi caps resolve: %w", err)
		}
		if cap.Capped() {
			sum += cap.Millicents
		} else {
			uncapped++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("kpi caps rows: %w", err)
	}
	return sum, uncapped, nil
}

// PerAppBudget is the layered budget block the trigger confirm (TRIG-03)
// renders: the app's resolved cap, today's spend, and the gate's verdict —
// one source of truth with the refusal, not a recomputation.
type PerAppBudget struct {
	AppID           string `json:"app_id"`
	CapKind         string `json:"cap_kind"`
	CapMillicents   int64  `json:"cap_millicents"`
	Capped          bool   `json:"capped"`
	SpentToday      int64  `json:"spent_today"`
	Remaining       int64  `json:"remaining_millicents"`
	RemainingCapped bool   `json:"remaining_capped"`
	Refused         bool   `json:"refused"`
	Reason          string `json:"reason,omitempty"`
}

// Budget loads the per-app layered budget block from the gate's own
// Evaluate aggregate (CONTEXT decision 1: one aggregate).
func (s *Service) Budget(ctx context.Context, appID string) (PerAppBudget, error) {
	if s.Pool == nil {
		return PerAppBudget{}, errors.New("postgres unavailable")
	}
	id, err := parseAppID(appID)
	if err != nil {
		return PerAppBudget{}, err
	}
	verdict, err := Evaluate(ctx, s.Pool, id)
	if err != nil {
		return PerAppBudget{}, fmt.Errorf("budget card: %w", err)
	}
	return PerAppBudget{
		AppID:           appID,
		CapKind:         string(verdict.Cap.Kind),
		CapMillicents:   verdict.Cap.Millicents,
		Capped:          verdict.Cap.Capped(),
		SpentToday:      verdict.Spend,
		Remaining:       verdict.Remaining,
		RemainingCapped: verdict.Cap.Capped(),
		Refused:         verdict.Refused,
		Reason:          verdict.Reason,
	}, nil
}

// KPIsHTTP serves GET /api/cockpit/kpis (view permission): the header
// aggregates, optionally narrowed to one app. A bad app filter is 422.
func (s *Service) KPIsHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.gate(w, r, false) {
		return
	}
	k, err := s.KPIs(r.Context(), r.URL.Query().Get("app_id"))
	if err != nil {
		if errors.Is(err, errInvalidRequest) {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		s.Logger.Error("cockpit kpi read failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "kpi read failed")
		return
	}
	writeWire(w, http.StatusOK, k)
}

// BudgetHTTP serves GET /api/cockpit/budget (view permission): the
// per-app layered budget block (app_id required).
func (s *Service) BudgetHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.gate(w, r, false) {
		return
	}
	appID := r.URL.Query().Get("app_id")
	if appID == "" {
		writeErr(w, http.StatusUnprocessableEntity, "app_id query parameter is required")
		return
	}
	b, err := s.Budget(r.Context(), appID)
	if err != nil {
		if errors.Is(err, errInvalidRequest) {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		s.Logger.Error("cockpit budget read failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "budget read failed")
		return
	}
	writeWire(w, http.StatusOK, b)
}
