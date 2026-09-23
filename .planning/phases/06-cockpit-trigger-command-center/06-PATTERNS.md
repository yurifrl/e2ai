# Phase 6: Cockpit trigger, Command Center, and webhook simulate - Pattern Map

**Mapped:** 2026-09-23
**Files analyzed:** 21 (new + modified)
**Analogs found:** 19 / 21 (2 partial-greenfield flagged: `cockpit_state` single-row table, feed UNION read model)

## Pattern Inventory

| New/Modified File | Role | Data Flow | Closest Analog | Match Quality |
|-------------------|------|-----------|----------------|---------------|
| `api/internal/app/migrations/0010_cockpit.sql` | migration | schema | `0007_hardening.sql` (constraint widen) + `0008_alerts.sql` (new table, closed vocab) + `0009_repair_proposals.sql` (attribution cols) | exact |
| `api/internal/cockpit/http.go` | controller | request-response | `api/internal/agents/http.go` | exact |
| `api/internal/cockpit/trigger.go` | service | CRUD (tx: gate→run insert→audit→enqueue) | `api/internal/ingest/dedupe.go` + `api/internal/suite/canary.go` | exact |
| `api/internal/cockpit/pause.go` | service | CRUD (guarded state flip) | `api/internal/runs/state.go` (Transition) + `api/internal/agents/http.go` (kill switch) | role-match |
| `api/internal/cockpit/audit.go` | model | CRUD (append-only insert) | `api/internal/plans/http.go` approval attribution | role-match |
| `api/internal/cockpit/feed.go` | model | batch (UNION read model) | `api/internal/runs/queries.go` (pagination) + `api/internal/trends/trends.go` (scan loop) | partial |
| `api/internal/cockpit/simulate.go` | service | event-driven (drives real ingest) | `api/internal/ingest/sweep.go` (hand-built RunSeed) + `api/internal/ingest/http.go` (HMAC/secret map) | exact |
| `api/internal/cockpit/{http,trigger,feed}_test.go` | test | request-response / batch | `api/internal/ingest/ingest_test.go` (fakeStore+httptest) | exact |
| `api/internal/cockpit/e2e_test.go` | test | batch (DB-backed) | `api/internal/ingest/e2e_test.go` | exact |
| `api/cmd/e2ai-api/main.go` (mod) | config | wiring | itself — `newMux` Mount pattern (`main.go:336-352`) | exact |
| `web/src/features/cockpit/CommandCenterPage.tsx` | component | request-response (polling) | `web/src/features/runs/RunsListPage.tsx` | exact |
| `web/src/features/cockpit/KpiCards.tsx` | component | request-response (polling) | `web/src/features/quality/TrendsPage.tsx` | role-match |
| `web/src/features/cockpit/TriggerPanel.tsx` | component | request-response (POST + arm-confirm) | `web/src/features/plans/ApprovalsQueuePage.tsx` (`decide`) | exact |
| `web/src/features/cockpit/SimulateCard.tsx` | component | request-response (POST, no confirm) | `web/src/features/plans/ApprovalsQueuePage.tsx` (single-click action) | role-match |
| `web/src/features/cockpit/AgentRail.tsx` | component | request-response (polling list) | `web/src/features/runs/RunsListPage.tsx` (table + chips) | exact |
| `web/src/lib/types.ts` (mod) | model | wire DTO | itself — extend `trigger_source` union (`types.ts:42`) | exact |
| `web/src/lib/api.ts` (mod) | utility | request-response | itself — `acknowledgeAlert` POST analog (`api.ts:217-219`) | exact |
| `web/src/app/routes.tsx` (mod) | route | n/a | itself — index swap (`routes.tsx:21-24`) | exact |
| `web/admin.module.json` (mod) | config | n/a | itself — add cockpit command (`admin.module.json:42`) | exact |
| `web/src/main.tsx` (mod) | config | n/a | itself — mock role map needs an edit-permission user (`main.tsx:48-53`) | exact |
| `web/src/i18n/locales/*.json` (mod, ×4) | config | n/a | itself — four-locale convention | exact |

---

## Pattern Assignments

### `api/internal/cockpit/http.go` (controller, request-response)

**Analog:** `api/internal/agents/http.go` — the closest write-surface (permission gate, domain error mapping, JSON helpers, one method per endpoint).

**Permission gate** (`agents/http.go:69-83`, plus the cockpit-relevant compound gate at `agents/http.go:99-103`):
```go
func requirePermission(w http.ResponseWriter, r *http.Request, p httpx.Permission) bool {
	identity, ok := httpx.IdentityFromContext(r.Context())
	switch {
	case !ok:
		...
// runnerGate composes: service token OR admin permission —
func (s *AgentsHTTP) runnerGate(w http.ResponseWriter, r *http.Request) bool {
	if s.serviceIdentity(r) {
		return true
	}
	return requirePermission(w, r, httpx.RunEditPermission)
}
```
Cockpit endpoints gate on `httpx.RunEditPermission` only (BFF identity, no service token). Permission constants live in `api/internal/platform/httpx/auth.go:34-38` (`RunViewPermission = "admin.e2ai.view"`, `RunEditPermission = "admin.e2ai.edit"`, `RunAdminPermission` already defined and unused — no new permission needed).

**Error mapping — sentinel → HTTP status** (`agents/http.go:109-131`):
```go
func mapDomainError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "agent session not found")
	...
	// budget breaches / transition rejections → 409 (the kill-switch contract)
	...
	default:
		writeErr(w, http.StatusInternalServerError, fallback)
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":` + strconv.Quote(msg) + `}`))
}

func writeJSON(w http.ResponseWriter, status int, body any) { ... }
```
The domain error → 409 mapping (terminal-state rejections, budget breaches) is the exact contract the trigger gate and pause endpoint reuse: paused/budget-blocked trigger = 409 with the reason in `error`.

**Mount pattern** (`plans/http.go:540-548` — every surface mounts itself; main.go stays thin):
```go
func (s *PlansHTTP) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/plans", s.ListPlans)
	...
	mux.HandleFunc("POST /api/apps/{app}/plans-sync", s.TriggerSync)
}
```
Method-prefixed patterns (`"POST /api/..."`) make PathValue resolve and keep the surface honest — copy verbatim for `POST /api/cockpit/trigger`, `POST /api/cockpit/pause`, `POST /api/cockpit/simulate`, `GET /api/cockpit/feed`.

**Attribution read** (`plans/approvals` pattern, `approvals/http.go` `resolvedUser` ~`:287-292`):
```go
func resolvedUser(r *http.Request) string {
	if identity, ok := httpx.IdentityFromContext(r.Context()); ok && identity.User != "" {
		return identity.User
	}
	return httpx.UserOf(r)
}
```

### `api/internal/cockpit/trigger.go` (service, CRUD — one tx)

**Analog:** `api/internal/ingest/dedupe.go` (transactional triple-write) + `api/internal/suite/canary.go` (programmatic run insert — creating a run WITHOUT a webhook).

**Tx template** (`dedupe.go:121-125` — `Ingest` opens one tx; run row + delivery + enqueue commit together or not at all):
```go
func (s *Store) Ingest(ctx context.Context, seed RunSeed, queue JobEnqueuer) (Result, error) {
	tx, err := s.pool.Begin(ctx)
	...
	// delivery record + runs row + wait-rollout enqueue in ONE tx
```
Manual trigger must be the same shape: budget-gate read → run insert → audit insert → execute enqueue, all inside one `pool.Begin`/`Commit`, so a crash never leaves an un-attributed run.

**Run insert with programmatic id** (`canary.go:123-162` — THE manual-trigger precedent; canary is the only shipped code that creates runs outside the webhook path):
```go
	// Negative epoch-millisecond workflow_run_id: canary runs never collide
	// with webhook ids under the (app_id, workflow_run_id) unique key, and
	// are trivially greppable in SQL as run_number = 0.
	insert into runs
		(app_id, workflow_run_id, run_number, head_sha, environment,
		 trigger_source, status, started_at)
	... 'reconcile', 'provisioning', now()
```
Manual trigger copies this shape: no webhook delivery id → use a distinct id scheme (`delivery_id = 'manual:<operator>:<uuid>'`) and `trigger_source = 'manual'`; keep `run_number` 0 with a synthetic negative `workflow_run_id` (same collision-proof trick, greppable the same way). Status starts at `'queued'` (webhook path's first state — runs/state.go machine), NOT `'provisioning'` (canary skips the rollout gate by definition; a manual run should go through it like a webhook run).

**Trigger-source constants** (`dedupe.go:23-28` — add `TriggerManual = "manual"` here, same file that owns the vocabulary):
```go
// Trigger sources (runs.trigger_source).
const (
	TriggerWebhook   = "webhook"
	TriggerReconcile = "reconcile"
	TriggerSweep     = "sweep" // edge-sweep triggered runs (EDGE-01, 0007)
)
```
And widen the DB check constraint exactly as `0007_hardening.sql:59-63` did for `sweep`:
```sql
-- runs.trigger_source gains 'sweep' (EDGE-01 sweep-triggered runs). 0001 has
-- only a comment, no check constraint — this adds the real one...
alter table runs add constraint runs_trigger_source_check
  check (trigger_source in ('webhook', 'reconcile', 'sweep'));
```
→ 0010 must `drop constraint runs_trigger_source_check` and re-add with `'manual'` appended.

**Execute enqueue opts** (`rollout.go:53-59`, mirrored by `canary.go:186-196`):
```go
func waitRolloutOpts() *river.InsertOpts {
	return &river.InsertOpts{
		MaxAttempts: 1,
		UniqueOpts: river.UniqueOpts{ByState: []rivertype.JobState{
			rivertype.JobStateScheduled,
			rivertype.JobStatePending,
			rivertype.JobStateAvailable,
		...}}}
}
```
Manual trigger enqueues `e2ai:run:execute` (`ExecuteArgs{RunID: runID.String()}`, `suite/execute.go:53-56`) on the `runner` queue — same one-shot/UniqueOpts shape as canary (`canary.go:186-196`) so a double-click cannot double-enqueue.

**Budget gate** — the enforcement seam is `api/internal/agents/budget.go` (step ceiling + accumulator enforced inside the step-insert tx, `markBudgetExhausted` terminal flip). The cockpit gate is a PRE-flight read of the same accumulator: `sum(agent_sessions.cost_millicents)` joined through runs for the app, UTC day-scoped. Uncapped app = "no cap" state, never a refusal (CONTEXT decision). No shipped query does a day-scoped sum — the gate SQL is mint-fresh but trivial; the *contract* (409 + reason, not refusal semantics) comes from `agents` mapDomainError.

### `api/internal/cockpit/pause.go` (service, CRUD)

**Analog:** `api/internal/runs/state.go` — guarded transition.

**Guarded status update** (`state.go:99-` plus `TransitionError` at `:73-76`):
```go
func Transition(ctx context.Context, db DB, id pgtype.UUID, to Status) error {
	var current Status
	err := db.QueryRow(ctx, `select status from runs where id = $1`, id).Scan(&current)
	...
type TransitionError struct {
	From   Status
	To     Status
	Reason string // "forbidden by transition table" | "status changed concurrently"
}
```
The lost-race re-read produces a typed error with the *now* status — copy this for pause/resume on `cockpit_state` (single-row `paused boolean` / reason / `paused_by` / `paused_at`): pause must be a guarded `update ... where not paused` returning rows, concurrent-pause loser gets a typed conflict. Kill-switch precedent for "forced status + note": `agents/http.go` Kill handler (BUDG-02, `RunEditPermission` forces `status=killed`).

### `api/internal/cockpit/audit.go` (model, CRUD append-only)

**Analog:** approvals attribution — `plan_approvals` columns + write path (`plans/http.go:2` comment, attribution via `resolvedUser`, `approvals/http.go` decision insert). Migration shape from `0009_repair_proposals.sql`:
```sql
  proposed_by text not null,
  decided_by text,                       -- null until a human decides (report-only)
  decided_at timestamptz,
```
→ `audit_events(actor text not null, action text not null, ... created_at timestamptz default now())`, insert inside the trigger tx, never updated afterward (append-only like webhook_events).

### `api/internal/cockpit/feed.go` (model, batch)

**Partial analogs:** pagination from `runs/queries.go:65-89`:
```go
type Page struct {
	Limit  int
	Offset int
}
const (
	defaultListLimit = 50
	maxListLimit     = 200
)
func ListRuns(ctx context.Context, db DB, appID pgtype.UUID, page Page) ([]Run, error) {
	... order by created_at desc, id desc limit $2 offset $3
```
(tie-break by id desc = stable pages — copy verbatim for the feed); scan-loop discipline from `trends/trends.go:170-176` (`HistoryRecent`, rows.Err checked, newest-first query reversed into oldest-first series). The UNION-over-runs/agent-sessions/webhook-events read model itself has no shipped precedent — greenfield SQL (see No Analog). Agent-session list shape comes from `agents/http.go` queue listing.

### `api/internal/cockpit/simulate.go` (service, event-driven)

**Analog:** `api/internal/ingest/sweep.go:138-149` (hand-built RunSeed entering the real ingest seam) + the HMAC/secret machinery.

**Load-bearing correction for the plan:** `ingest.HandleEvent` (named in RESEARCH.md and 06-UI-SPEC.md:191) **does not exist** in the codebase. The real seam is:
```go
// dedupe.go:111-121
type Store struct {
	pool *pgxpool.Pool
}
func (s *Store) Ingest(ctx context.Context, seed RunSeed, queue JobEnqueuer) (Result, error)
// RunSeed fields (dedupe.go:95-106): App, DeliveryID, WorkflowRunID, RunNumber,
// HeadSHA, HeadBranch, PRNumber, TriggerSource, SignatureValid, Payload, Enqueue
```
Simulate = render a payload, HMAC-sign it server-side, then either (a) call `Store.Ingest` directly with a hand-built `RunSeed` (sweep.go precedent, `TriggerSource: TriggerReconcile` at `sweep.go:144-145`), or (b) go through the full HTTP path. Prefer (a): sweep.go proves the seed-construction pattern, and the HMAC round-trip stays exercisable via `verifySignature` in tests.

**HMAC verify** (`ingest/http.go:284-300`):
```go
func verifySignature(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	const prefix = "sha256="
	...
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
```
**Per-app secret map** (`ingest/http.go:120-125` — `WithSecretResolver` resolves op:// refs at boot into the in-memory map; resolver itself is `ingest/secrets.go` `OpSecretResolver`): the simulate endpoint signs with the same resolved map entry — **secrets never leave the server** (UI-SPEC contract: no secret fields in the browser).

**Synthetic payload precedent** — `ingest/e2e_test.go:231-232` (`dispatchRun(88001, "[BetWeb] Deploy to staging", "workflow_dispatch", "success")`) is the payload-shape factory simulate should reuse in tests; `webhookPayload` struct at `ingest/http.go:303-320` is the narrow decode target.

**Filter semantics:** `seedFromWebhook` (`ingest/http.go:334-348`) returns filtered payloads with `Enqueue` semantics tied to the filter — simulate of a non-workflow_run kind should record-without-run exactly as the real handler does (ResultRecorded path).

### `api/cmd/e2ai-api/main.go` (modified — wiring)

**Analog:** itself. `newMux` registers every surface with a comment naming the phase/requirement (`main.go:336-352`):
```go
	if approvals != nil {
		// Approvals surface (APPR-01/02): queue + attributed decisions.
		approvals.Mount(mux)
	}
```
Cockpit surface mounts the same way, same `if cockpit != nil` guard, same nil-degrades posture. Worker/wiring order (config → pool → river → surfaces → mux → server) is already the shape to extend.

---

### `web/src/features/cockpit/CommandCenterPage.tsx` (component, polling)

**Analog:** `web/src/features/runs/RunsListPage.tsx` — index page + `usePolling` + incremental list.

**Polling loader shape** (`RunsListPage.tsx:23-24`):
```tsx
const loader = useMemo(() => () => listRuns(PAGE_SIZE, 0), [])
const firstPage = usePolling(loader, REFRESH_MS)
```
**The hook** (`usePolling.ts:23-27, 58-59, 76-77`) — re-runs loader on interval, stops on `document.hidden`, previous payload survives errors, deps `[enabled, loader, intervalMs]`:
```tsx
loader: () => Promise<T>,
intervalMs: number,
enabled = true,
keepGoing?: (data: T) => boolean,
```
All cockpit polling (feed, KPIs, pause state) goes through this one hook — no `setInterval` hand-rolls. Feed keeps polling while the trigger panel is open (UI-SPEC B: "opening it pauses nothing").

**Action POST with busy/error/409 states** (`ApprovalsQueuePage.tsx:26-31, 51-75`) — the exact shape for TriggerPanel and SimulateCard:
```tsx
const [busyId, setBusyId] = useState<string | null>(null)
const [actionError, setActionError] = useState<string | null>(null)
const queue = usePolling(getApprovalsQueue, 5000)
...
const decide = async (entry: PendingApproval, decision: 'approve' | 'reject') => {
	setBusyId(entry.version.id)
	setActionError(null)
	try {
		...
	} finally {
		setBusyId(null)
	}
}
```
A 409 means state changed while the operator was reading — surface inline, let the next poll refresh it away. TriggerPanel's budget-blocked/paused states reuse this verbatim.

**Taxonomy chips** — house `StatusChip`/`VerdictChip` from `web/src/features/runs/chips.tsx` are canonical for run status/verdict coloring in the feed; UI-SPEC:54 locks this (don't switch to `StatusBadge` from admin-ui for run taxonomy).

### `web/src/lib/api.ts` + `web/src/lib/types.ts` (modified)

**POST convention** (`api.ts:217-219` — newest POST in the file):
```ts
export function acknowledgeAlert(id: string): Promise<{ acknowledged: boolean }> {
  return postJson(`/api/trends/alerts/${encodeURIComponent(id)}/acknowledge`, {})
}
```
Cockpit calls follow: `postJson('/api/cockpit/trigger', {...})`, `postJson('/api/cockpit/pause', {...})`, `postJson('/api/cockpit/simulate', {...})`, `getJson('/api/cockpit/feed?...')`. Header doc-comment lists the endpoint contract — extend it, don't scatter.

**Union widening** (`types.ts:42`):
```ts
trigger_source: 'WEBHOOK' | 'RECONCILE'
```
→ add `'MANUAL'`. Casing rule: DB stores lowercase (`trigger_source` column, `dedupe.go:25-27`), wire is UPPERCASE (`report/page.go:51` `TriggerSource string \`json:"trigger_source"\``) — the type union is the single point where the casing lives; grep `isTriggerSource`-style helpers when extending.

### `web/src/app/routes.tsx` + `web/admin.module.json` (modified)

**Index swap** (`routes.tsx:21-24` — Command Center becomes the landing page, RunsList moves to `/e2ai/runs`):
```tsx
    {
      index: true,
      element: <RunsListPage />,
    },
```
**Nav/command registration** (`admin.module.json:42-` — `commands` array with `id`, localized `title`, `route`, `icon` (lucide name), `section`, `keywords`; add the cockpit entry per UI-SPEC Copywriting Contract). Permissions block (`admin.module.json` `"permissions": {"view": ["admin.e2ai.view"]}`) — cockpit stays behind the same `admin.e2ai.view`; *edit actions* are API-gated, not nav-gated.

### Dev-identity wiring (modified: `web/src/main.tsx`, `web/vite.config.ts`)

Both dev paths currently grant **view only** — every cockpit mutation (trigger/pause/simulate) will 403 in standalone dev until this is widened:

`main.tsx:48-53`:
```ts
const mockUsers = createMockUsersFromRoles({
  admin: ['admin.e2ai.view'],
  editor: ['admin.e2ai.view'],
  viewer: ['admin.e2ai.view'],
  noAccess: [],
})
```
`vite.config.ts:73-76`:
```ts
          headers: {
            'X-User': 'admin@example.com',
            'X-Permissions': 'admin.e2ai.view',
          },
```
Fix: `editor` mock user gains `'admin.e2ai.edit'` (and vite forwards `admin.e2ai.view,admin.e2ai.edit` — comma-separated per `httpx/auth.go` `parsePermissions`). Keep `viewer` view-only so permission-gating remains testable in dev.

### `web/src/i18n/locales/*.json` (modified, ×4)

Every cockpit string is a key in all four locales (`en-US`, `pt-BR`, `es`, `ro` — file list verified); registered via `registerModuleTranslations('e2ai', ...)` in `main.tsx:33-39` and `t('e2ai.*')` consumed via `useI18n('e2ai')`. No hardcoded English in JSX (UI-SPEC:216). New feature keys follow the existing per-area nesting (`e2ai.runs.*`, `e2ai.approvals.*`) → `e2ai.cockpit.*`.

---

## Shared Patterns

### Permission ladder (apply to every cockpit endpoint)
**Source:** `api/internal/platform/httpx/auth.go:34-38` + `agents/http.go:70-83` + `plans/http.go:120-123`
```go
RunViewPermission  Permission = "admin.e2ai.view"
RunEditPermission  Permission = "admin.e2ai.edit"
RunAdminPermission Permission = "admin.e2ai.admin"
...
func gate(w http.ResponseWriter, r *http.Request, edit bool) bool {
	need := httpx.RunViewPermission
	if edit {
		need = httpx.RunEditPermission
	}
```
Reads (feed) → view; writes (trigger/pause/simulate) → edit. Cockpit triggers audit attribution from the same `RequestIdentity` (`httpx.IdentityFromContext`).

### Error contract (apply to all cockpit handlers)
**Source:** `agents/http.go:109-131`
Sentinel `errors.Is` → 404 unknown id / 409 state conflicts (paused, budget breached, transition rejected) / 500 fallback. `writeErr`/`writeJSON` verbatim. The 409 contract is the kill-switch contract (BUDG-02) — pause and trigger reuse it.

### Transactional mutation (apply to trigger; audit rides inside)
**Source:** `dedupe.go:121-125` — `pool.Begin` → run insert (`on conflict do nothing` guard, `dedupe.go:213-219`) → audit insert → `queue.Insert` → commit. Everything commits together or not at all.

### River enqueue (apply to trigger + simulate handoff)
**Source:** `canary.go:186-196` / `rollout.go:53-59` — `Queue: "runner"`, `MaxAttempts: 1`, `UniqueOpts.ByState` covering scheduled/pending/available/running. JobEnqueuer injected as interface (`suite/execute.go:89-90`), late-bound in main.go like `QueueRef` (`dedupe.go:69-72`).

### Wire DTO casing (apply to all new DTOs)
**Source:** `runs/queries.go` (DB lowercase snake_case) → `types.ts` (UPPERCASE unions, snake_case keys) — trigger_source is the live example. New cockpit DTOs: `paused` boolean, `budget` block, `trigger_source: 'WEBHOOK' | 'RECONCILE' | 'MANUAL'`.

### i18n (apply to every user-visible string)
**Source:** `admin.module.json` translations + `main.tsx:24-39` registration. Four locales, every phase, no exceptions.

### Tests
**HTTP-surface tests:** `ingest/ingest_test.go:98-131` — `fakeStore`/`fakeEnqueuer` seams + `httptest.NewRecorder` + `newTestHandler`; error-path table at `:314-321` (store error → 500). CockpitHTTP gets the same shape with a fake store — the fakeStore seam (`ingest_test.go:104-107`) is the template.
**DB-backed e2e:** `ingest/e2e_test.go:231-249` — dispatch payload + `select id from runs where workflow_run_id = 88001` assertion shape; skips silently without `E2AI_TEST_DATABASE_URL` (ingest_test.go:362-364). Manual-trigger e2e asserts on the synthetic negative id the same way.
**Seed helpers:** `runs/queries_test.go` `seedApp`/`seedRun` for status-machine tests.
**Concurrency:** `canary_test.go:92-95` — concurrent-trigger dedupe test pattern for double-click protection.

---

## No Analog Found (greenfield — planner must design, not copy)

| File/Concern | Role | Data Flow | Reason |
|------|------|-----------|--------|
| `cockpit_state` single-row table + guarded pause | model | CRUD | No shipped single-row mutable-state table exists (everything else is append-only or row-per-entity). The *guarded flip* pattern exists (`state.go` Transition) but the table shape is minted in 0010. |
| Feed UNION read model (`feed.go`) | model | batch | No UNION-over-multiple-event-tables query exists anywhere; trends aggregates one table (`HistoryRecent`). Copy pagination + scan discipline, invent the UNION. |
| Day-scoped budget-gate sum (UTC) | query | read | No day-scoped accumulator query exists; budget.go does per-session tx enforcement. Gate SQL is new; contract (409 + reason, uncapped = pass-through) is decided in CONTEXT. |
| `audit_events` table | model | append-only | No audit table exists; closest is `plan_approvals` attribution columns and `webhook_events` append-only posture. |

Everything else maps to a shipped analog above.

## Metadata

**Analog search scope:** `api/internal/{ingest,suite,agents,plans,approvals,runs,trends,platform/httpx,app/migrations}`, `api/cmd/e2ai-api`, `web/src/{features,lib,app,i18n/locales}`, `web/{admin.module.json,vite.config.ts}`
**Files scanned:** 24 read in full or in targeted ranges this session
**Pattern extraction date:** 2026-09-23
**Corrections vs upstream docs:** `ingest.HandleEvent` (RESEARCH.md, 06-UI-SPEC.md:191) does not exist — the seam is `ingest.Store.Ingest` + hand-built `RunSeed` (sweep.go precedent). Plans referencing HandleEvent must say `Store.Ingest`.