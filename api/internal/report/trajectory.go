// T3 (phase 5): the agentic trajectory slice of the report page blob.
// Reads agent_sessions + session_steps (0004) straight off the pool — the
// same read-only posture as the runs reads above. No agents package import:
// the report surface is a leaf consumer, and the wire DTOs here are the
// report page's own (additive on types.ts, mirrored by T6). Deterministic
// Playwright runs have no agent_sessions rows and load to nil, so their
// wire shape is unchanged.
package report

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// loadTrajectory assembles the run's full agent trajectory plus its cost
// summary. Sessions read oldest-first (created_at, id — the same order the
// claim scan and admin queue see); steps read in idx order. A session with
// more steps than wireMaxLimit keeps its most recent wireMaxLimit entries
// and flags truncated; steps_used on the session header stays authoritative.
// No sessions → (nil, nil); the caller leaves the JSON keys null.
func loadTrajectory(ctx context.Context, pool *pgxpool.Pool, runID pgtype.UUID) ([]trajectorySessionDTO, *agentSummaryDTO, error) {
	rows, err := pool.Query(ctx, `
		select id, role, status, steps_used, cost_millicents
		from agent_sessions
		where run_id = $1
		order by created_at, id
	`, runID)
	if err != nil {
		return nil, nil, err
	}
	type sessRow struct {
		id  pgtype.UUID
		dto trajectorySessionDTO
	}
	var out []trajectorySessionDTO
	var ids []pgtype.UUID
	for rows.Next() {
		var r sessRow
		var role, status string
		var cost int64
		if err := rows.Scan(&r.id, &role, &status, &r.dto.StepsUsed, &cost); err != nil {
			rows.Close()
			return nil, nil, err
		}
		r.dto.SessionID = r.id.String()
		r.dto.Role = role
		r.dto.Status = status
		r.dto.CostMillicents = cost
		r.dto.Steps = []trajectoryStepDTO{}
		out = append(out, r.dto)
		ids = append(ids, r.id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(out) == 0 {
		return nil, nil, nil
	}

	var summary agentSummaryDTO
	summary.Sessions = make([]agentSummarySessionDTO, 0, len(out))
	for i := range out {
		s := &out[i]
		summary.Sessions = append(summary.Sessions, agentSummarySessionDTO{
			Role:           s.Role,
			Status:         s.Status,
			CostMillicents: s.CostMillicents,
		})
		summary.TotalCostMillicents += s.CostMillicents

		steps, err := loadSessionSteps(ctx, pool, runID, ids[i], s)
		if err != nil {
			return nil, nil, err
		}
		s.Steps = steps
	}
	return out, &summary, nil
}

// loadSessionSteps reads one session's trajectory in idx order, capping the
// wire payload at the most recent wireMaxLimit steps.
func loadSessionSteps(ctx context.Context, pool *pgxpool.Pool, runID pgtype.UUID, sessionID pgtype.UUID, s *trajectorySessionDTO) ([]trajectoryStepDTO, error) {
	rows, err := pool.Query(ctx, `
		select idx, kind, decision, action, evidence, tokens_in, tokens_out, cost_millicents, created_at
		from session_steps
		where session_id = $1
		order by idx
	`, sessionID)
	if err != nil {
		return nil, err
	}
	var steps []trajectoryStepDTO
	for rows.Next() {
		var st trajectoryStepDTO
		var evidence []byte
		var createdAt pgtype.Timestamp
		if err := rows.Scan(&st.Idx, &st.Kind, &st.Decision, &st.Action, &evidence, &st.TokensIn, &st.TokensOut, &st.CostMillicents, &createdAt); err != nil {
			rows.Close()
			return nil, err
		}
		st.Evidence = wireEvidence(runID, evidence)
		st.CreatedAt = createdAt.Time.UTC().Format(time.RFC3339Nano)
		steps = append(steps, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(steps) > wireMaxLimit {
		s.Truncated = true
		steps = steps[len(steps)-wireMaxLimit:]
	}
	return steps, nil
}

// wireEvidence decodes the session_steps evidence jsonb array
// ([{kind,path}]) and stamps each ref with its token-gated artifact page
// URL. Paths that fail sanitizeArtifactPath keep kind/path but no URL —
// evidence the viewer can show but not link (SECR-01 traversal safety).
func wireEvidence(runID pgtype.UUID, raw []byte) []evidenceRefDTO {
	var refs []struct {
		Kind string `json:"kind"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &refs); err != nil {
		return []evidenceRefDTO{}
	}
	out := []evidenceRefDTO{}
	for _, r := range refs {
		ref := evidenceRefDTO{Kind: r.Kind, Path: r.Path}
		if safe, ok := sanitizeArtifactPath(r.Path); ok {
			u := artifactPageURL(runID, safe)
			ref.URL = &u
		}
		out = append(out, ref)
	}
	return out
}
