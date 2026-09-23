// Agent rail (COCK-03): the live session list under the Command Center —
// one row per non-terminal agent session, oldest heartbeats first, with
// the same >90s stuck predicate the admin queue renders
// (agents/http.go: claimed|running sessions whose heartbeat went quiet).
// Terminal sessions (completed/failed/killed) drop off the rail; history
// lives in the feed.
package cockpit

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
)

// RailSession is one live agent session on the rail (wire contract locked
// by the plan).
type RailSession struct {
	RunID               string `json:"run_id"`
	Role                string `json:"role"`
	Status              string `json:"status"`
	HeartbeatAgeSeconds int64  `json:"heartbeat_age_seconds"`
	Stuck               bool   `json:"stuck"`
}

// AgentRail loads the live (non-terminal) sessions across all apps,
// optionally narrowed to one app via its runs.
func (s *Service) AgentRail(ctx context.Context, appID string) ([]RailSession, error) {
	if s.Pool == nil {
		return nil, errors.New("postgres unavailable")
	}

	args := []any{}
	// One where clause: the app filter (optional) and the live-status
	// predicate always combine — two bare WHEREs would be invalid SQL.
	live := "se.status in ('queued', 'claimed', 'running', 'budget_exhausted')"
	where := "where " + live
	if appID != "" {
		id, err := parseAppID(appID)
		if err != nil {
			return nil, err
		}
		args = append(args, id)
		where = "where r.app_id = $1 and " + live
	}

	rows, err := s.Pool.Query(ctx, `
		select se.run_id, se.role, se.status,
		       extract(epoch from now() - coalesce(se.heartbeat_at, se.created_at))::bigint
		         as heartbeat_age_seconds,
		       se.status in ('claimed', 'running')
		         and coalesce(se.heartbeat_at, se.created_at) < now() - interval '90 seconds' as stuck
		from agent_sessions se
		join runs r on r.id = se.run_id
		`+where+`
		order by coalesce(se.heartbeat_at, se.created_at), se.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("agent rail query: %w", err)
	}
	defer rows.Close()

	var out []RailSession
	for rows.Next() {
		var sess RailSession
		var runID pgtype.UUID
		if err := rows.Scan(&runID, &sess.Role, &sess.Status,
			&sess.HeartbeatAgeSeconds, &sess.Stuck); err != nil {
			return nil, fmt.Errorf("agent rail scan: %w", err)
		}
		sess.RunID = uuidString(runID)
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent rail rows: %w", err)
	}
	if out == nil {
		out = []RailSession{}
	}
	return out, nil
}

// RailHTTP serves GET /api/cockpit/agents (view permission): the live
// session rail, optionally narrowed to one app. A bad app filter is 422.
func (s *Service) RailHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.gate(w, r, false) {
		return
	}
	sessions, err := s.AgentRail(r.Context(), r.URL.Query().Get("app_id"))
	if err != nil {
		if errors.Is(err, errInvalidRequest) {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		s.Logger.Error("cockpit agent rail read failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "agent rail read failed")
		return
	}
	writeWire(w, http.StatusOK, map[string]any{"sessions": sessions})
}
