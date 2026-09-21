// Package corpus owns the expected-behavior oracle (CORP-01/02): a per-app set
// of behaviors that plans reference via `uses:` keys, plus an append-only
// stream of unverified learnings from the report stage. Writes flow through a
// deliberately narrow set of functions — spec seeding (trusted, human-authored
// sources), learning append, and human-only promote/dismiss — so no pipeline
// stage can self-mutate the oracle: run evidence only ever lands as an
// unverified learning awaiting a human decision.
package corpus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// corpus_behaviors.status values (check constraint in 0002_plans.sql).
const (
	BehaviorUnverified = "unverified"
	BehaviorVerified   = "verified"
	BehaviorStale      = "stale"
)

// corpus_learnings.status values (check constraint in 0002_plans.sql).
const (
	LearningUnverified = "unverified"
	LearningPromoted   = "promoted"
	LearningDismissed  = "dismissed"
)

// Provenance kinds (corpus_behaviors.provenance.kind).
const (
	ProvenancePlaywrightSpec = "playwright-spec"
	ProvenanceRun            = "run"
	ProvenanceLearning       = "learning"
)

// Behavior mirrors one corpus_behaviors row: a falsifiable expected behavior
// with its source provenance. Status transitions happen only through the
// write functions below (seed → verified; human verify → verified/stale).
type Behavior struct {
	ID          pgtype.UUID
	AppID       pgtype.UUID
	Key         string // stable per-app reference key, e.g. PWSPEC-LIVE-BADGE
	Title       string
	Description string // falsifiable expected behavior
	Status      string
	Provenance  Provenance
	CreatedAt   pgtype.Timestamptz
	VerifiedBy  pgtype.Text
	VerifiedAt  pgtype.Timestamptz
}

// Learning mirrors one corpus_learnings row: an agent observation recorded
// during the report stage. Append-only — observations never change; the only
// allowed transitions are unverified → promoted (PromoteLearning) or →
// dismissed (DismissLearning), both human-initiated.
type Learning struct {
	ID          pgtype.UUID
	AppID       pgtype.UUID
	RunID       pgtype.UUID // nullable: observations need not pin a run
	Observation string
	Status      string
	CreatedAt   pgtype.Timestamptz
}

// Provenance is the JSONB provenance block on corpus_behaviors: where the
// expected behavior came from. Kind playwright-spec pins a spec path + commit;
// kind learning pins the originating learning (and its run when known).
type Provenance struct {
	Kind       string `json:"kind"` // playwright-spec | run | learning
	Path       string `json:"path,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	LearningID string `json:"learning_id,omitempty"`
	Commit     string `json:"commit,omitempty"`
}

// Sentinel errors surfaced by the store; callers (REST handlers) map them to
// 404 / 409 responses.
var (
	ErrNotFound         = errors.New("corpus: row not found")
	ErrAlreadyPromoted  = errors.New("corpus: learning already promoted")
	ErrAlreadyDismissed = errors.New("corpus: learning already dismissed")
	ErrEmptyObservation = errors.New("corpus: learning observation must not be empty")
)

// UpsertBehavior inserts a behavior, or — when (app_id, key) already exists —
// refreshes title/description/status/provenance from the incoming row. It is
// the single write path for behaviors (seed and promotion both use it), which
// keeps the (app_id, key) idempotency invariant in one place.
func UpsertBehavior(ctx context.Context, pool *pgxpool.Pool, b *Behavior) error {
	if b == nil {
		return fmt.Errorf("corpus: behavior must not be nil")
	}
	if !b.AppID.Valid {
		return fmt.Errorf("corpus: behavior %q requires app_id", b.Key)
	}
	for _, field := range []struct{ name, value string }{
		{"key", b.Key}, {"title", b.Title}, {"description", b.Description}, {"status", b.Status},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("corpus: behavior requires non-empty %s", field.name)
		}
	}
	if b.Provenance.Kind == "" {
		return fmt.Errorf("corpus: behavior %q requires provenance.kind", b.Key)
	}
	prov, err := json.Marshal(b.Provenance)
	if err != nil {
		return fmt.Errorf("corpus: marshal provenance for %q: %w", b.Key, err)
	}
	_, err = pool.Exec(ctx, `
		insert into corpus_behaviors
			(app_id, key, title, description, status, provenance, verified_by, verified_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (app_id, key) do update set
			title = excluded.title,
			description = excluded.description,
			status = excluded.status,
			provenance = excluded.provenance,
			verified_by = excluded.verified_by,
			verified_at = excluded.verified_at`,
		b.AppID, b.Key, b.Title, b.Description, b.Status, prov, b.VerifiedBy, b.VerifiedAt)
	if err != nil {
		return fmt.Errorf("corpus: upsert behavior %q: %w", b.Key, err)
	}
	return nil
}

const behaviorColumns = `id, app_id, key, title, description, status, provenance, created_at, verified_by, verified_at`

// scanBehaviors decodes a behaviors result set (queries supply the order by).
func scanBehaviors(rows pgx.Rows) ([]Behavior, error) {
	defer rows.Close()
	var out []Behavior
	for rows.Next() {
		var b Behavior
		var prov []byte
		if err := rows.Scan(&b.ID, &b.AppID, &b.Key, &b.Title, &b.Description,
			&b.Status, &prov, &b.CreatedAt, &b.VerifiedBy, &b.VerifiedAt); err != nil {
			return nil, fmt.Errorf("corpus: scan behavior: %w", err)
		}
		if err := json.Unmarshal(prov, &b.Provenance); err != nil {
			return nil, fmt.Errorf("corpus: unmarshal behavior provenance: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("corpus: iterate behaviors: %w", err)
	}
	return out, nil
}

// ListBehaviors returns the app's behaviors ordered by key (stable for UI and
// `uses:` validation).
func ListBehaviors(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID) ([]Behavior, error) {
	rows, err := pool.Query(ctx, `
		select `+behaviorColumns+` from corpus_behaviors where app_id = $1 order by key`,
		appID)
	if err != nil {
		return nil, fmt.Errorf("corpus: list behaviors: %w", err)
	}
	return scanBehaviors(rows)
}

// GetBehavior returns the behavior with the given per-app key, or nil when
// absent (the seam plan validation uses to resolve `uses:` references).
func GetBehavior(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID, key string) (*Behavior, error) {
	rows, err := pool.Query(ctx, `
		select `+behaviorColumns+` from corpus_behaviors where app_id = $1 and key = $2`,
		appID, key)
	if err != nil {
		return nil, fmt.Errorf("corpus: get behavior %q: %w", key, err)
	}
	behaviors, err := scanBehaviors(rows)
	if err != nil {
		return nil, err
	}
	if len(behaviors) == 0 {
		return nil, nil
	}
	return &behaviors[0], nil
}

// AppendLearning records an unverified agent observation from the report
// stage. Append-only by construction: the observation is written once and
// never updated afterwards — only the human-only promote/dismiss transitions
// below touch a learning again.
func AppendLearning(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID, runID pgtype.UUID, observation string) (*Learning, error) {
	observation = strings.TrimSpace(observation)
	if observation == "" {
		return nil, ErrEmptyObservation
	}
	l := &Learning{AppID: appID, RunID: runID, Observation: observation, Status: LearningUnverified}
	err := pool.QueryRow(ctx, `
		insert into corpus_learnings (app_id, run_id, observation)
		values ($1, $2, $3)
		returning id, status, created_at`,
		appID, runID, observation).Scan(&l.ID, &l.Status, &l.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("corpus: append learning: %w", err)
	}
	return l, nil
}

// ListLearnings returns the app's learnings newest first (the promotion
// queue surface).
func ListLearnings(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID) ([]Learning, error) {
	rows, err := pool.Query(ctx, `
		select id, app_id, run_id, observation, status, created_at
		from corpus_learnings where app_id = $1 order by created_at desc, id desc`,
		appID)
	if err != nil {
		return nil, fmt.Errorf("corpus: list learnings: %w", err)
	}
	defer rows.Close()
	var out []Learning
	for rows.Next() {
		var l Learning
		if err := rows.Scan(&l.ID, &l.AppID, &l.RunID, &l.Observation, &l.Status, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("corpus: scan learning: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("corpus: iterate learnings: %w", err)
	}
	return out, nil
}

// GetLearning loads one learning by id, or ErrNotFound. The REST layer
// re-reads a learning after a decision so the client sees the new status.
func GetLearning(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID) (*Learning, error) {
	var l Learning
	err := pool.QueryRow(ctx, `
		select id, app_id, run_id, observation, status, created_at
		from corpus_learnings where id = $1`, id).
		Scan(&l.ID, &l.AppID, &l.RunID, &l.Observation, &l.Status, &l.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("corpus: get learning: %w", err)
	}
	return &l, nil
}

// PromoteLearning copies a learning into the behaviors oracle as
// human-verified: the new behavior carries provenance.kind=learning (pinning
// the originating learning and its run), verified_by set to the promoting
// user, and the learning flips to promoted — atomically, in one transaction.
// Promoting is a one-shot decision: already-decided learnings return
// ErrAlreadyPromoted / ErrAlreadyDismissed.
func PromoteLearning(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID, verifiedBy string) (*Behavior, error) {
	if strings.TrimSpace(verifiedBy) == "" {
		return nil, fmt.Errorf("corpus: promote requires an attributed user (CORP-02: human-only writes)")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("corpus: begin promote: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	b, err := promoteLearningTx(ctx, tx, id, verifiedBy)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("corpus: commit promote: %w", err)
	}
	return b, nil
}

// promoteLearningTx is the promote body over an open transaction — shared by
// the manual REST flow and the repair-proposal approval path, so approval can
// never introduce a second corpus write semantics.
func promoteLearningTx(ctx context.Context, tx pgx.Tx, id pgtype.UUID, verifiedBy string) (*Behavior, error) {
	var l Learning
	err := tx.QueryRow(ctx, `
		select app_id, run_id, observation, status from corpus_learnings where id = $1`, id).
		Scan(&l.AppID, &l.RunID, &l.Observation, &l.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("corpus: load learning for promote: %w", err)
	}
	switch l.Status {
	case LearningPromoted:
		return nil, ErrAlreadyPromoted
	case LearningDismissed:
		return nil, ErrAlreadyDismissed
	}

	behaviorKey := learningBehaviorKey(id)
	prov := Provenance{Kind: ProvenanceLearning, LearningID: id.String()}
	if l.RunID.Valid {
		prov.RunID = l.RunID.String()
	}
	provJSON, err := json.Marshal(prov)
	if err != nil {
		return nil, fmt.Errorf("corpus: marshal promote provenance: %w", err)
	}

	b := &Behavior{}
	var rawProv []byte
	err = tx.QueryRow(ctx, `
		insert into corpus_behaviors
			(app_id, key, title, description, status, provenance, verified_by, verified_at)
		values ($1, $2, $3, $4, $5, $6, $7, now())
		on conflict (app_id, key) do update set
			title = excluded.title,
			description = excluded.description,
			status = excluded.status,
			provenance = excluded.provenance,
			verified_by = excluded.verified_by,
			verified_at = excluded.verified_at
		returning `+behaviorColumns,
		l.AppID, behaviorKey, learningTitle(l.Observation), l.Observation, BehaviorVerified,
		provJSON, verifiedBy).Scan(
		&b.ID, &b.AppID, &b.Key, &b.Title, &b.Description, &b.Status,
		&rawProv, &b.CreatedAt, &b.VerifiedBy, &b.VerifiedAt)
	if err != nil {
		return nil, fmt.Errorf("corpus: promote insert behavior: %w", err)
	}
	if err := json.Unmarshal(rawProv, &b.Provenance); err != nil {
		return nil, fmt.Errorf("corpus: unmarshal promote provenance: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		update corpus_learnings set status = $1 where id = $2 and status = $3`,
		LearningPromoted, id, LearningUnverified)
	if err != nil {
		return nil, fmt.Errorf("corpus: promote flip learning: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Lost a race against a concurrent decision.
		return nil, ErrAlreadyPromoted
	}
	return b, nil
}

// DismissLearning marks an unverified learning dismissed — a terminal,
// human-only decision. The observation row is never deleted (append-only
// audit trail); it just stops being promotable.
func DismissLearning(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID) error {
	tag, err := pool.Exec(ctx, `
		update corpus_learnings set status = $1 where id = $2 and status = $3`,
		LearningDismissed, id, LearningUnverified)
	if err != nil {
		return fmt.Errorf("corpus: dismiss learning: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var status string
	err = pool.QueryRow(ctx, `select status from corpus_learnings where id = $1`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("corpus: load learning for dismiss: %w", err)
	}
	if status == LearningPromoted {
		return ErrAlreadyPromoted
	}
	return ErrAlreadyDismissed
}

// learningBehaviorKey derives the promoted behavior's key from the learning
// id: deterministic, stable across retries, and collision-free (ids are
// unique), so promotion never races on key selection.
func learningBehaviorKey(id pgtype.UUID) string {
	return "LEARN-" + strings.ToUpper(id.String()[:8])
}

// learningTitle condenses the observation's first line into the behavior
// title column (bounded so long observations stay UI-friendly); the full
// observation becomes the falsifiable description.
func learningTitle(observation string) string {
	first, _, _ := strings.Cut(observation, "\n")
	first = strings.TrimSpace(first)
	runes := []rune(first)
	if len(runes) > 120 {
		first = string(runes[:120]) + "…"
	}
	return first
}
