// Store reads behind the plans REST surface (T4). The write path lives in
// sync.go (upsertPlanVersion); this file is the list/detail/content/diff
// side that feeds the web module. Every query returns wire-adjacent rows
// — the DTO mapping lives in http.go so the query layer stays JSON-free.
package plans

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is surfaced by the queries here; REST maps it to 404.
// (Distinct from the corpus sentinel errors on purpose: those gate
// promote/dismiss, these gate plan/version reads.)
var ErrNotFound = errors.New("plans: row not found")

// ErrNoPlansSource marks a registered app with no plans_source configured —
// the queue worker skips it silently; the REST trigger surfaces it.
var ErrNoPlansSource = errors.New("plans: app has no plans_source")

// planRow is one plans table row plus its frontmatter-derived display
// fields, joined from the latest version.
type planRow struct {
	ID       string
	AppID    pgtype.UUID
	App      string // owner/repo of the registered app
	Title    *string
	Surfaces []string
	Surface  *string // nil on pre-phase-4 rows whose frontmatter lacks surface
	Trigger  *string
	Status   string
	SrcPath  string
	LatestID *string
	Updated  time.Time
}

// versionRow is one plan_versions row (the version-history summary; the
// content endpoint re-reads content/frontmatter separately).
type versionRow struct {
	ID        pgtype.UUID
	PlanID    string
	Version   string
	Hash      string
	Status    string
	CreatedBy string
	CreatedAt time.Time
}

// ListPlans returns plans (optionally scoped to one app, optionally
// filtered by plans.status) newest-updated first, paginated by offset.
func ListPlans(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID, status string, limit, offset int) ([]planRow, error) {
	sql := `
		select p.id, p.app_id, a.owner || '/' || a.repo, f.title, f.surfaces, f.surface, f.trigger,
		       p.status, p.source_path, p.latest_version_id, p.updated_at
		from plans p
		join registered_apps a on a.id = p.app_id
		left join plan_versions v on v.id = p.latest_version_id
		left join lateral (select * from jsonb_to_record(v.frontmatter)
			as x(title text, surfaces text[], surface text, trigger text)) f on v is not null
	`
	args := []any{}
	if appID.Valid {
		sql += " where p.app_id = $" + strconv.Itoa(len(args)+1)
		args = append(args, appID)
	}
	if status != "" {
		if appID.Valid {
			sql += " and p.status = $" + strconv.Itoa(len(args)+1)
		} else {
			sql += " where p.status = $" + strconv.Itoa(len(args)+1)
		}
		args = append(args, status)
	}
	sql += " order by p.updated_at desc, p.id limit $" + strconv.Itoa(len(args)+1) +
		" offset $" + strconv.Itoa(len(args)+2)
	args = append(args, limit, offset)

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPlanRows(rows)
}

func scanPlanRows(rows pgx.Rows) ([]planRow, error) {
	var out []planRow
	for rows.Next() {
		var r planRow
		var surfaces []string
		if err := rows.Scan(&r.ID, &r.AppID, &r.App, &r.Title, &surfaces, &r.Surface, &r.Trigger,
			&r.Status, &r.SrcPath, &r.LatestID, &r.Updated); err != nil {
			return nil, err
		}
		if surfaces != nil {
			r.Surfaces = surfaces
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetPlan loads one plan row by id, or ErrNotFound.
func GetPlan(ctx context.Context, pool *pgxpool.Pool, planID string) (*planRow, error) {
	rows, err := pool.Query(ctx, `
		select p.id, p.app_id, a.owner || '/' || a.repo, f.title, f.surfaces, f.surface, f.trigger,
		       p.status, p.source_path, p.latest_version_id, p.updated_at
		from plans p
		join registered_apps a on a.id = p.app_id
		left join plan_versions v on v.id = p.latest_version_id
		left join lateral (select * from jsonb_to_record(v.frontmatter)
			as x(title text, surfaces text[], surface text, trigger text)) f on v is not null
		where p.id = $1`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	got, err := scanPlanRows(rows)
	if err != nil {
		return nil, err
	}
	if len(got) == 0 {
		return nil, ErrNotFound
	}
	return &got[0], nil
}

// ListPlanVersions returns the version history of one plan, oldest first
// (the detail page renders it as a timeline).
func ListPlanVersions(ctx context.Context, pool *pgxpool.Pool, planID string) ([]versionRow, error) {
	rows, err := pool.Query(ctx, `
		select id, plan_id, version, content_hash, status, created_by, created_at
		from plan_versions where plan_id = $1 order by created_at asc, version asc`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanVersionRows(rows)
}

func scanVersionRows(rows pgx.Rows) ([]versionRow, error) {
	var out []versionRow
	for rows.Next() {
		var r versionRow
		if err := rows.Scan(&r.ID, &r.PlanID, &r.Version, &r.Hash, &r.Status,
			&r.CreatedBy, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// versionContent is the stored markdown plus its frontmatter document.
type versionContent struct {
	ID      pgtype.UUID
	PlanID  string
	Version string
	Content string
	FM      []byte // raw jsonb frontmatter document
	Hash    string
	Status  string
}

// GetVersionContent loads one version by id, or ErrNotFound.
func GetVersionContent(ctx context.Context, pool *pgxpool.Pool, vid pgtype.UUID) (*versionContent, error) {
	var v versionContent
	err := pool.QueryRow(ctx, `
		select id, plan_id, version, content, frontmatter, content_hash, status
		from plan_versions where id = $1`, vid).
		Scan(&v.ID, &v.PlanID, &v.Version, &v.Content, &v.FM, &v.Hash, &v.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// DiffBase picks the baseline for a version diff: the explicit request
// base when given (must belong to the same plan → ErrNotFound otherwise),
// else the version stored just before it (same plan, greatest created_at
// below it). No previous version → empty baseline with no base id.
func DiffBase(ctx context.Context, pool *pgxpool.Pool, vid pgtype.UUID, baseVID *pgtype.UUID) (content string, baseID *pgtype.UUID, err error) {
	if baseVID != nil {
		var samePlan bool
		err := pool.QueryRow(ctx, `
			select v1.plan_id = v2.plan_id, v2.content
			from plan_versions v1 join plan_versions v2 on v2.id = $2
			where v1.id = $1`, vid, *baseVID).Scan(&samePlan, &content)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, ErrNotFound
		}
		if err != nil {
			return "", nil, err
		}
		if !samePlan {
			return "", nil, ErrNotFound
		}
		return content, baseVID, nil
	}
	var prev string
	err = pool.QueryRow(ctx, `
		select v2.content
		from plan_versions v1
		join plan_versions v2 on v2.plan_id = v1.plan_id and v2.created_at < v1.created_at
		where v1.id = $1
		order by v2.created_at desc limit 1`, vid).Scan(&prev)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil, nil // no baseline: empty diff
	case err != nil:
		return "", nil, err
	}
	return prev, nil, nil
}

// ResolveApp maps an app ref (uuid, owner/repo, owner/repo/env, or
// workflow name) to a registered app id, or ErrNotFound. Mirrors
// lookupPlanApp's grammar but takes the HTTP-level contract.
func ResolveApp(ctx context.Context, pool *pgxpool.Pool, ref string) (pgtype.UUID, error) {
	if id, err := parseUUID(ref); err == nil {
		return id, nil
	}
	var id pgtype.UUID
	err := pool.QueryRow(ctx, `
		select id from registered_apps
		where owner || '/' || repo = $1
		   or owner || '/' || repo || '/' || environment = $1
		   or workflow_name = $1
		limit 1`, ref).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, ErrNotFound
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	return id, nil
}

// AppRef returns the owner/repo string for an app id.
func AppRef(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID) (string, error) {
	var ref string
	err := pool.QueryRow(ctx,
		`select owner || '/' || repo from registered_apps where id = $1`, appID).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return ref, err
}

