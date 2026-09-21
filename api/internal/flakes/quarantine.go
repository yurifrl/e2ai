// Quarantine store (FLAK-01/02): the guarded mutations over test_quarantine.
// Activate re-opens a lifted row via a conditional upsert (one row per
// (app_id, file, title) identity, ever), Lift is a guarded status transition
// and Expire is the sweep's bulk lifter — nothing here mutates the app under
// test; the store only flips its own rows (report-only).
package flakes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Quarantine lifecycle sentinels; HTTP maps them (404/409/400).
var (
	ErrNotFound           = errors.New("flakes: quarantine entry not found")
	ErrAlreadyQuarantined = errors.New("flakes: test is already quarantined")
	ErrNotQuarantined     = errors.New("flakes: test is not quarantined")
	ErrInvalidInput       = errors.New("flakes: invalid quarantine request")
)

// Who quarantined the test. Agent rows are the pipeline's proposals (the
// report stage acts on them by skipping the test; nothing mutates the app).
const (
	ByAgent        = "agent"
	ByHuman        = "human"
	LiftedBySystem = "system" // Expire's bulk lifter, not a human attribution
)

func validQuarantiner(by string) bool {
	return by == ByAgent || by == ByHuman
}

// Entry is one test_quarantine row.
type Entry struct {
	ID               string     `json:"id"`
	AppID            string     `json:"app_id"`
	File             string     `json:"file"`
	Title            string     `json:"title"`
	Reason           string     `json:"reason"`
	EvidenceArtifact string     `json:"evidence_artifact,omitempty"`
	Status           string     `json:"status"` // active | lifted
	QuarantinedBy    string     `json:"quarantined_by"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	LiftedAt         *time.Time `json:"lifted_at,omitempty"`
	LiftedBy         string     `json:"lifted_by,omitempty"`
}

// Activate quarantines a test identity: the report stage skips it until the
// entry is lifted or expires. Re-activating a lifted row re-opens it with the
// new reason/evidence (same identity, new episode); activating an already
// active row is a conflict — lift it first.
func Activate(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID,
	file, title, reason, evidence, by string, expiresAt *time.Time) (*Entry, error) {
	if !validQuarantiner(by) {
		return nil, fmt.Errorf("%w: quarantined_by must be agent or human", ErrInvalidInput)
	}
	if title == "" || reason == "" {
		return nil, fmt.Errorf("%w: title and reason are required", ErrInvalidInput)
	}

	var id pgtype.UUID
	err := pool.QueryRow(ctx, `
		insert into test_quarantine
			(app_id, file, title, reason, evidence_artifact, quarantined_by, expires_at)
		values ($1, $2, $3, $4, nullif($5, ''), $6, $7)
		on conflict (app_id, file, title) do update set
			reason = excluded.reason,
			evidence_artifact = excluded.evidence_artifact,
			quarantined_by = excluded.quarantined_by,
			expires_at = excluded.expires_at,
			status = 'active',
			created_at = now(),
			lifted_at = null,
			lifted_by = null
		where test_quarantine.status = 'lifted'
		returning id
	`, appID, file, title, reason, evidence, by, orNullTime(expiresAt)).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The upsert guard fired: the identity row is already active.
		return nil, ErrAlreadyQuarantined
	case err != nil:
		return nil, fmt.Errorf("flakes: activate quarantine: %w", err)
	}
	return getByID(ctx, pool, id)
}

// Lift lifts an active quarantine by id, attributing the action. An
// already-lifted row is a conflict (ErrNotQuarantined), an unknown id a 404.
// Lifts are human or system actions — a username (or LiftedBySystem) is
// expected, never the agent constants (agents propose, humans lift).
func Lift(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID, by string) (*Entry, error) {
	if by == "" || by == ByAgent {
		return nil, fmt.Errorf("%w: lifted_by must be a human attribution", ErrInvalidInput)
	}
	tag, err := pool.Exec(ctx, `
		update test_quarantine
		set status = 'lifted', lifted_at = now(), lifted_by = $2
		where id = $1 and status = 'active'
	`, id, by)
	if err != nil {
		return nil, fmt.Errorf("flakes: lift quarantine: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := pool.QueryRow(ctx,
			`select exists (select 1 from test_quarantine where id = $1)`, id).Scan(&exists); err != nil {
			return nil, fmt.Errorf("flakes: check quarantine exists: %w", err)
		}
		if !exists {
			return nil, ErrNotFound
		}
		return nil, ErrNotQuarantined
	}
	return getByID(ctx, pool, id)
}

// Expire bulk-lifts every active quarantine past its expiry (the reconcile
// sweep's closing step). Returns the number of rows lifted.
func Expire(ctx context.Context, pool *pgxpool.Pool, now time.Time) (int64, error) {
	tag, err := pool.Exec(ctx, `
		update test_quarantine
		set status = 'lifted', lifted_at = $2, lifted_by = $3
		where status = 'active' and expires_at is not null and expires_at <= $2
	`, now, LiftedBySystem)
	if err != nil {
		return 0, fmt.Errorf("flakes: expire quarantines: %w", err)
	}
	return tag.RowsAffected(), nil
}

// List returns the quarantine rows for an app (zero appID = every app),
// filtered by status ("active", "lifted" or "" for both).
func List(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID, status string) ([]Entry, error) {
	if status != "" && status != "active" && status != "lifted" {
		return nil, fmt.Errorf("%w: status must be active, lifted or empty", ErrInvalidInput)
	}
	rows, err := pool.Query(ctx, `
		select id, app_id, file, title, reason, evidence_artifact, status,
		       quarantined_by, created_at, expires_at, lifted_at, lifted_by
		from test_quarantine
		where ($1::uuid is null or app_id = $1)
		  and ($2::text = '' or status = $2)
		order by created_at desc
	`, appID, status)
	if err != nil {
		return nil, fmt.Errorf("flakes: list quarantine: %w", err)
	}
	defer rows.Close()

	entries, err := scanEntries(rows)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func getByID(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID) (*Entry, error) {
	rows, err := pool.Query(ctx, `
		select id, app_id, file, title, reason, evidence_artifact, status,
		       quarantined_by, created_at, expires_at, lifted_at, lifted_by
		from test_quarantine
		where id = $1
	`, id)
	if err != nil {
		return nil, fmt.Errorf("flakes: get quarantine: %w", err)
	}
	defer rows.Close()
	entries, err := scanEntries(rows)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, ErrNotFound
	}
	return &entries[0], nil
}

func scanEntries(rows pgx.Rows) ([]Entry, error) {
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		var e Entry
		var evidence, liftedBy pgtype.Text
		var expiresAt, liftedAt pgtype.Timestamptz
		var id, appID pgtype.UUID
		if err := rows.Scan(&id, &appID, &e.File, &e.Title, &e.Reason,
			&evidence, &e.Status, &e.QuarantinedBy, &e.CreatedAt,
			&expiresAt, &liftedAt, &liftedBy); err != nil {
			return nil, fmt.Errorf("flakes: scan quarantine row: %w", err)
		}
		e.ID, e.AppID = pgUUIDString(id), pgUUIDString(appID)
		if evidence.Valid {
			e.EvidenceArtifact = evidence.String
		}
		if liftedBy.Valid {
			e.LiftedBy = liftedBy.String
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			e.ExpiresAt = &t
		}
		if liftedAt.Valid {
			t := liftedAt.Time
			e.LiftedAt = &t
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("flakes: iterate quarantine rows: %w", err)
	}
	return entries, nil
}

func orNullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}
