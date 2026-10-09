// Command Center feed (COCK-02): the reverse-chronological activity stream
// stitched from the three locked sources — runs, agent sessions, and alert
// arrivals — into one wire shape (T-06-11). Pagination is keyset on
// (created_at, id) descending, not OFFSET: a stream fed live by ingest
// would otherwise shift rows between pages and skip or repeat entries. The
// cursor is opaque base64 of the pair; the optional app_id filter narrows
// to one app. Sessions and alerts contribute projection fields only —
// never session steps, alert payloads, or any other raw content.
package cockpit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// FeedLimits bounds the feed page; the default/cap pairing mirrors the
// runs list (runs.Page bounds).
const (
	feedDefaultLimit = 50
	feedMaxLimit     = 200
)

// FeedEntryKind discriminates the stream source on the wire.
type FeedEntryKind string

const (
	FeedKindRun     FeedEntryKind = "run"
	FeedKindSession FeedEntryKind = "agent_session"
	FeedKindAlert   FeedEntryKind = "alert"
)

// FeedEntry is one feed row: identity, source kind, ordering stamp, the
// app (and run where applicable) it belongs to, and a pre-rendered one-line
// summary. No nested payloads.
type FeedEntry struct {
	ID        string        `json:"id"`
	Kind      FeedEntryKind `json:"kind"`
	CreatedAt string        `json:"created_at"`
	AppID     string        `json:"app_id"`
	RunID     string        `json:"run_id,omitempty"`
	Summary   string        `json:"summary"`
}

// FeedPage is one feed slice plus the continuation cursor; cursor is empty
// on the last page.
type FeedPage struct {
	Entries []FeedEntry `json:"entries"`
	Cursor  string      `json:"cursor,omitempty"`
}

// feedCursor is the decoded keyset pair.
type feedCursor struct {
	Before time.Time
	ID     string
}

// encodeCursor renders the keyset pair opaque.
func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id))
}

// decodeCursor parses an opaque cursor back into the keyset pair.
func decodeCursor(s string) (feedCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return feedCursor{}, fmt.Errorf("%w: malformed feed cursor", errInvalidRequest)
	}
	before, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return feedCursor{}, fmt.Errorf("%w: malformed feed cursor", errInvalidRequest)
	}
	t, err := time.Parse(time.RFC3339Nano, before)
	if err != nil {
		return feedCursor{}, fmt.Errorf("%w: malformed feed cursor", errInvalidRequest)
	}
	return feedCursor{Before: t, ID: id}, nil
}

// Feed stitches the three sources into one reverse-chronological page. The
// UNION ALL projects every source onto the same column shape, orders by
// the keyset pair, and hands back limit+1 rows' worth of continuation
// (limit+1 probe trimmed before rendering the cursor).
func (s *Service) Feed(ctx context.Context, limit int, appID string, cursor string) (FeedPage, error) {
	if s.Pool == nil {
		return FeedPage{}, errors.New("postgres unavailable")
	}
	if limit <= 0 {
		limit = feedDefaultLimit
	}
	if limit > feedMaxLimit {
		limit = feedMaxLimit
	}

	args := []any{limit + 1}
	var tmpl []string
	if appID != "" {
		id, err := parseAppID(appID)
		if err != nil {
			return FeedPage{}, err
		}
		args = append(args, id)
		tmpl = append(tmpl, fmt.Sprintf("{APP} = $%d", len(args)))
	}
	var cur feedCursor
	if cursor != "" {
		c, err := decodeCursor(cursor)
		if err != nil {
			return FeedPage{}, err
		}
		cur = c
		args = append(args, cur.Before, cur.ID)
		tmpl = append(tmpl, fmt.Sprintf("({PAIR}) < ($%d::timestamptz, $%d::uuid)", len(args)-1, len(args)))
	}
	// The predicate is built once with {APP}/{PAIR} placeholders and
	// qualified per branch: the sessions arm joins runs, and both tables
	// carry created_at/id, so unqualified names are ambiguous (42702).
	qualify := func(app, pair string) string {
		if len(tmpl) == 0 {
			return ""
		}
		re := strings.NewReplacer("{APP}", app, "{PAIR}", pair)
		return " where " + re.Replace(strings.Join(tmpl, " and "))
	}
	whereRuns := qualify("r.app_id", "r.created_at, r.id")
	whereSessions := qualify("r.app_id", "se.created_at, se.id")
	whereAlerts := qualify("a.app_id", "a.created_at, a.id")

	// One pass, one plan: every branch projects (created_at, id, kind,
	// app_id, run_id, summary). A run contributes exactly one event per
	// row — the runs table has no updated_at column (0001/0007), so
	// re-renders at finish are row revisions, not new events.
	rows, err := s.Pool.Query(ctx, `
		(select r.created_at, r.id, 'run' as kind, r.app_id, r.id::text as run_id,
		        case when r.environment <> '' then
		            r.environment || ' run ' || r.workflow_run_id::text || ' — ' || upper(r.status)
		        else 'run ' || r.workflow_run_id::text || ' — ' || upper(r.status) end as summary
		 from runs r`+whereRuns+`
		 order by r.created_at desc, r.id desc
		 limit $1)
		union all
		(select se.created_at, se.id, 'agent_session' as kind, r.app_id, se.run_id::text,
		        se.role || ' session ' || se.status as summary
		 from agent_sessions se join runs r on r.id = se.run_id`+whereSessions+`
		 order by se.created_at desc, se.id desc
		 limit $1)
		union all
		(select a.created_at, a.id, 'alert' as kind, a.app_id, '' as run_id,
		        a.kind || ' alert' as summary
		 from alert_events a`+whereAlerts+`
		 order by a.created_at desc, a.id desc
		 limit $1)
		order by created_at desc, id desc
		limit $1`, args...)
	if err != nil {
		return FeedPage{}, fmt.Errorf("feed query: %w", err)
	}
	defer rows.Close()

	var out []FeedEntry
	for rows.Next() {
		var e FeedEntry
		var id, appIDv pgtype.UUID
		var runID pgtype.Text
		var createdAt pgtype.Timestamptz
		if err := rows.Scan(&createdAt, &id, &e.Kind, &appIDv, &runID, &e.Summary); err != nil {
			return FeedPage{}, fmt.Errorf("feed scan: %w", err)
		}
		e.ID = uuidString(id)
		e.AppID = uuidString(appIDv)
		e.CreatedAt = createdAt.Time.UTC().Format(time.RFC3339Nano)
		e.RunID = nullableTextString(runID)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return FeedPage{}, fmt.Errorf("feed rows: %w", err)
	}
	if out == nil {
		out = []FeedEntry{}
	}

	page := FeedPage{Entries: out}
	if len(out) > limit {
		last := out[limit-1]
		page.Entries = out[:limit]
		t, err := time.Parse(time.RFC3339Nano, last.CreatedAt)
		if err != nil {
			return FeedPage{}, fmt.Errorf("feed cursor stamp: %w", err)
		}
		page.Cursor = encodeCursor(t, last.ID)
	}
	return page, nil
}

// --- tiny scan helpers shared by the read models (kpis.go, agents.go) ---

// FeedHTTP serves GET /api/cockpit/feed (view permission): optional limit,
// app_id, and cursor query parameters. A bad cursor or app filter is 422 —
// the shape of the wire request, not the data.
func (s *Service) FeedHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.gate(w, r, false) {
		return
	}
	q := r.URL.Query()
	appID := q.Get("app_id")
	if appID != "" {
		if _, err := parseAppID(appID); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeErr(w, http.StatusUnprocessableEntity, "limit must be a positive integer")
			return
		}
		limit = n
	}
	page, err := s.Feed(r.Context(), limit, appID, q.Get("cursor"))
	if err != nil {
		if errors.Is(err, errInvalidRequest) {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		s.Logger.Error("cockpit feed read failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "feed read failed")
		return
	}
	writeWire(w, http.StatusOK, page)
}

// parseAppID validates the app_id query parameter into a pgtype.UUID.
func parseAppID(raw string) (pgtype.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%w: app_id is not a uuid", errInvalidRequest)
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

// uuidString renders a scanned uuid for the wire; nulls render empty.
func uuidString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return uuid.UUID(id.Bytes).String()
}

// nullableTextString renders a scanned text for the wire; nulls render empty.
func nullableTextString(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}
