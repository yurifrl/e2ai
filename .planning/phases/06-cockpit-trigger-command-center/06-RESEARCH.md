# Phase 6: Cockpit trigger, Command Center, and webhook simulate - Research

**Researched:** 2026-09-23
**Domain:** Cockpit control-plane substrate (manual trigger + budget gate + pause state + audit) over the shipped report-only QA pipeline (Go net/http + River v0.47.0 + Postgres + React 18 admin module)
**Confidence:** HIGH — every load-bearing claim below was verified against this repo's code this session with file:line citations. MEDIUM items are explicitly tagged (staging BFF scopes, executor first-transition detail, pause scope ambiguity).

**Milestone research reused (not restated):** `.planning/research/STACK.md` (zero-new-deps inventory, River API verification, module layout), `.planning/research/ARCHITECTURE.md` (integration points, cancel propagation), `.planning/research/PITFALLS.md` (v1.0 amplification table), `.planning/research/SUMMARY.md`. River v0.47.0 API semantics (`JobList`, `JobCancel`, `QueuePause/Resume`, `UniqueOpts`) are verified in STACK.md against the vendored module source — this file only adds the phase-6-specific seams.

<user_constraints>
## User Constraints (from CONTEXT.md)

Verbatim from `.planning/phases/06-cockpit-trigger-command-center/06-CONTEXT.md` (lines 12–22). These are locked; research did not explore alternatives.

### Locked Decisions

1. **Budget caps (BUDG-03):** per-app cap lives as a nullable `budget_cap_millicents` column on the app row in `0010_cockpit.sql` (editable later via P9 registry); a global default cap env var (`E2AI_DEFAULT_BUDGET_CAP_MILLICENTS`) floors uncapped apps. Uncapped = explicit "no cap" state in the confirm UI — never a refusal. Gate = `sum(agent_sessions.cost_millicents)` joined via runs for the app over the current UTC day vs cap; breach → 409 with remaining-budget reason; UI pre-flight shows spend/remaining. (06-CONTEXT.md:12)
2. **Global pause (COCK-04):** single-row `cockpit_state` table in 0010 (paused flag, reason, operator, updated_at). Permission rung = `admin.e2ai.edit`. Pause blocks **both** manual triggers and webhook simulate (paused = the arm accepts no new runs, full stop); pause/unpause is an audited mutation like every cockpit write; state surfaces in the KPI header + banner. In-flight runs are untouched (report-only). (06-CONTEXT.md:13)
3. **Manual run shape (TRIG-01..05):** mirror the deploy path 1:1 — trigger endpoint inserts the run row (`trigger_source='manual'`, synthetic id from a dedicated sequence — fail-closed uniqueness regression test proves pre-migration constraint rejection) + audit row in one tx, then enqueues `suite.ExecuteArgs` on the runner queue (`ingest/rollout.go:149` shape). No new execution semantics; no auto agent-session enqueue (P8 adds the attach action; today's sessions attach via existing `POST /api/runs/{id}/agent-sessions`). (06-CONTEXT.md:14)
4. **Command Center feed (COCK-02):** read-model UNION endpoint over existing tables (runs, agent_sessions, alert_events) — no `feed_events` write path, no write amplification; all event sources verified present (scout §10). Day-scoped KPIs (runs today, pass rate today) minted in the cockpit domain (`trends.HistoryRecent` is window-scoped, not day-scoped); pending-approvals badge reuses `GET /api/approvals`; budget KPI uses the same per-app/day aggregate as the gate. (06-CONTEXT.md:15)

### Locked substrate facts (06-CONTEXT.md:19-22)

- `0010_cockpit.sql` is the next migration (0009 is highest). Contents: widen `runs_trigger_source_check` (0007:59-63 pattern) with `'manual'`; audit attribution table (clean mint — **no audit table exists today**; closest in-repo pattern `plan_approvals.approver/decided_at` written via `httpx.UserOf`); `cockpit_state` pause row; app budget-cap column; dedicated synthetic-id sequence.
- Trigger endpoint `POST /api/cockpit/triggers` `{app_id, head_sha, head_branch?, environment, reason?}` gated `httpx.RunEditPermission` (first consumer of `admin.e2ai.edit`; pattern: `agents/http.go` Enqueue + `approvals/http.go:267` attribution). Rejects: zero-step approved plans, ceiling-0 (validation error, engine sentinel untouched), paused arm, budget breach, zero-approved-plan apps (→ link to Approvals). Concurrent triggers serialize: one 202, one 409.
- Sessions minted by the trigger path always carry explicit positive ceilings (`step_ceiling = len(plan.Steps)`, cost ceiling from app cap) — P1.
- Webhook simulate (ACTN-01): server-side endpoint reusing the real ingest seam (`ingest.HandleEvent`); api renders payload + HMAC-signs with the app's resolved secret (secrets never reach the browser); result = ingest outcome + created-run link. Simulate ingest outcomes are visible in Command Center. Blocked when paused.

### Discretion (unchanged)
- Mobile surfaces untouched (Phase 4 shipped them; cockpit is web + api).
- No auto-approval — Approvals queue stays the only approval path.

### Deferred (OUT OF SCOPE)
- Auto agent-session enqueue on manual trigger (P8 "Add agent session" action covers attachment).
- Per-app cap editing UI (P9 registry wizard; cap column + env floor land now).
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|------------------|
| COCK-01 | Admin landing is the Command Center at the module root (sketch 001-A built for real): KPI header (runs today, pass rate today, pending approvals badge, budget spend vs cap, paused/kill-switch state), reverse-chron live feed, agent rail (REQUIREMENTS.md:115) | Index-route swap in `web/src/app/routes.tsx` (index currently → RunsListPage); KPI sources verified (§ Key Facts 8); feed UNION endpoint (Decision D6) |
| COCK-02 | Feed rows expand inline to a compact detail strip (verdict, duration, cost, evidence thumbnails); app filter chips constrain the feed (REQUIREMENTS.md:116) | Feed read model over existing tables; cost from `agent_sessions.cost_millicents`; filters client-side over UNION payload |
| COCK-03 | [partial — planner pulls verbatim from .planning/REQUIREMENTS.md:114-130] Command Center affordance set (simulate entry / agent rail detail per sketch 001) | Simulate seam (§ Key Facts 7); sessions list pattern agents/http.go:300-360 |
| COCK-04 | Global pause/kill-switch state: `cockpit_state` single row, edit-rung pause/unpause, KPI + banner surfacing, blocks new runs | § Key Facts 4; decision D4 (pause enforcement scope) needs user confirm |
| TRIG-01..05 | Manual trigger: endpoint + pre-flight gate UI + confirm (budget, uncapped-explicit) + run in feed + reason | § Key Facts 2/3; endpoint contract locked at 06-CONTEXT.md:20 |
| ACTN-01 | Webhook simulate: server-side, real ingest seam, HMAC-signed with resolved app secret, outcome + run link, blocked when paused | § Key Facts 7; HMAC helper precedent ingest_test.go:79-82 |
| AUDT-01 | Audit trail for cockpit writes | § Key Facts 5 — no audit table exists; mint `cockpit_audit` in 0010 |
| BUDG-03 | Trigger-time budget gate: per-app cap column + env floor + UTC-day aggregate + 409 with remaining reason | § Key Facts 3; engine ceiling semantics verified budget.go:76-93 |
</phase_requirements>

## Summary

Phase 6 mints the cockpit substrate: migration `0010_cockpit.sql`, a manual-trigger endpoint that drives the **standard** pipeline (no new execution semantics), a single-row global pause, an audit table, and the Command Center as the module's landing page. The codebase work required is narrow and well-precedented: every mechanism the phase needs already exists in shipped code in exactly reusable form — transactional run insertion (`ingest/dedupe.go`), runner-queue enqueue with unique-by-state opts (`suite/canary.go:155-158`), permission gating (`httpx` ladder + `gate()` pattern), audit attribution (`approvals/http.go:250-273`), HMAC signing (test helper precedent), and read-model aggregation (`trends`, `alert_events`, `agent_sessions`).

The three genuine design problems are: (1) the runs table has **no nullable identity columns** — `workflow_run_id bigint not null`, `run_number int not null`, `unique (app_id, workflow_run_id)` (0001:39-40,52) — so manual runs need a collision-proof synthetic id from a dedicated sequence plus a `run_number` policy (Decision D2/D3); (2) the trigger-time budget gate must read **ceiling-0 semantics correctly** — the engine treats `0 = unlimited` (0004:27-28, budget.go:79,89), so the trigger boundary, not the engine, must refuse ceiling-0; (3) pause enforcement must decide its blast radius (cockpit surfaces only vs every run-creation seam) — Decision D4, flagged for user confirmation because decision 2's "full stop" parenthetical and its explicit two-surface enumeration point in different directions.

**Primary recommendation:** Build 0010 exactly per CONTEXT §substrate; implement `POST /api/cockpit/triggers` as insert-run-at-`waiting-rollout` + audit-row + `suite.ExecuteArgs` enqueue in one tx behind `RunEditPermission`, serialize concurrency with River `UniqueOpts` (precedent `canary.go:155-158`); implement simulate as a server-side HMAC-signed replay into the real ingest seam; add `MANUAL` (and the already-missing `SWEEP`) to the locked `types.ts:42` union by extension only.

## Architectural Responsibility Map

| Capability | Primary Tier | Secondary Tier | Rationale |
|------------|-------------|----------------|-----------|
| Manual trigger endpoint + tx | API (Go, `cockpit` package) | — | Runs row + audit + enqueue must commit atomically; BFF-identity server tier owns actuation |
| Trigger-time budget gate | API (cockpit domain) | — | UTC-day SQL aggregate over `agent_sessions`⋈`runs`; UI only *displays* spend |
| Global pause state | API (`cockpit_state`) | Web banner (read-only) | Single-row table is source of truth; web renders |
| Audit attribution | API (`cockpit_audit` + `resolvedUser`) | — | Server-side identity resolution (BFF headers); never client-supplied actor |
| Webhook simulate | API (wraps `ingest` seam) | — | HMAC secret must never reach browser; api renders + signs + calls real ingest |
| Command Center feed | API (UNION read endpoint) | Web (render, poll) | Read-model over existing tables; no write path (CONTEXT decision 4) |
| Day-scoped KPIs | API (cockpit domain) | Web | `trends.HistoryRecent` is window-scoped, not day-scoped — cockpit mints its own day queries |
| Permission visibility tiering | Web (frontend hiding) | API (enforcement) | auth.go:1-6 model: BFF authenticates, frontend hides per roles, API enforces |

## Standard Stack

**Zero new dependencies** — verified in `.planning/research/STACK.md` ("v1.1 requires zero new deps") and confirmed by this session's reads: every phase-6 need maps to an existing in-repo mechanism or a Go stdlib/River feature already vendored.

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| net/http ServeMux | Go 1.26.6 [VERIFIED: probe] | Routes with method prefixes + `{id}` wildcards | Shipped pattern: main.go:336-410 |
| riverqueue/river | v0.47.0 [VERIFIED: STACK.md, vendored source] | ExecuteArgs enqueue on runner queue; `UniqueOpts` concurrency gate | platform/river/river.go owns client construction; `SkipUnknownJobCheck: true` |
| jackc/pgx/v5 | shipped | tx-scoped inserts, `for update` locking (budget precedent) | ingest/dedupe.go, agents/budget.go |
| google/uuid | shipped | run id generation | runs/queries.go, approvals helpers |
| crypto/hmac + sha256 | Go stdlib | simulate payload signing | precedent: ingest_test.go:79-82 |
| @nsxbet/admin-sdk + React 18 | web/package.json | AdminShell, routes, i18n, mock auth | main.tsx wiring |
| fetch + usePolling | web/src/lib/usePolling.ts | polling with hidden-tab stop, keepGoing terminal states | verbatim behavior verified |

**Environment toolchain (probed 2026-09-23):** go 1.26.6 darwin/arm64, bun 1.4.2. Postgres :5432 was closed at probe time — local DB comes up under Tilt (Tiltfile per STACK.md). Docker/Tilt CLI presence not probed (guardrail-gated twice); op CLI confirmed present (secrets.go shells to `op read`).

## Package Legitimacy Audit

Not applicable — this phase installs **zero external packages** (CONTEXT decisions + STACK.md). No registry lookups, no legitimacy gate required. No `[ASSUMED]` package names appear anywhere in this document.

## Key Facts (Verified Seams)

### 1. Migration 0010 — substrate contents (locked, 06-CONTEXT.md:19)

Five pieces, in one additive migration following 0009's style (`api/internal/app/migrations/0009_repair_proposals.sql`: header comment, closed vocabularies, `check` constraints, desc indexes):

```sql
-- 0010_cockpit.sql (skeleton — shapes follow 0004/0007/0008/0009 conventions)

-- (a) widen trigger vocabulary: drop + re-add the 0007 constraint
alter table runs drop constraint runs_trigger_source_check;
alter table runs add constraint runs_trigger_source_check
  check (trigger_source in ('webhook', 'reconcile', 'sweep', 'manual'));

-- (b) per-app budget cap (nullable = uncapped; env var floors at read time)
alter table registered_apps add column budget_cap_millicents bigint;

-- (c) single-row pause state (0008-style closed shape, single-row via check)
create table cockpit_state (
  id int primary key default 1 check (id = 1),
  paused boolean not null default false,
  reason text not null default '',
  operator text not null default '',
  updated_at timestamptz not null default now()
);
insert into cockpit_state (id) values (1);

-- (d) audit attribution table (clean mint — none exists today, 06-CONTEXT.md:19)
create table cockpit_audit (
  id uuid primary key default gen_random_uuid(),
  actor text not null,                    -- resolvedUser(r), never client-supplied
  action text not null,                   -- closed vocabulary (check constraint)
  target_type text not null,
  target_id text not null default '',
  detail jsonb not null default '{}',
  created_at timestamptz not null default now()
);
create index cockpit_audit_created_idx on cockpit_audit (created_at desc);

-- (e) synthetic workflow_run_id sequence (polarity: Decision D3)
create sequence cockpit_manual_run_id_seq;
```

Note: a trigger-source value wider than the 0007 check is **rejected by the database, not the app** — this is the fail-closed regression the CONTEXT requires: a test inserting `trigger_source='manual'` must fail before 0010 and pass after (proves the widen is real and no other code path smuggled the value in earlier).

### 2. Runs schema — the manual-trigger crux

[VERIFIED: api/internal/app/migrations/0001_init.sql:27-53]

- `webhook_events`: `delivery_id text not null` (dedupe key), `workflow_run_id bigint not null`, `signature_valid boolean not null`, `payload jsonb not null`.
- `runs`: `app_id uuid not null references registered_apps(id)`, **`workflow_run_id bigint not null` (:39)**, **`run_number int not null` (:40)**, `head_sha text not null`, `head_branch text` (nullable), `environment text not null`, `trigger_source text not null default 'webhook'` (:43), **`unique (app_id, workflow_run_id)` (:52)**, index `(app_id, created_at desc)` (:54).

Consequences for the trigger endpoint:
- Both `workflow_run_id` **and** `run_number` must be synthesized (CONTEXT names the sequence for the id; `run_number` has no uniqueness constraint but is `not null int` — Decision D2).
- Insert path is `insertRun`'s SQL shape verbatim (`ingest/dedupe.go:195-218`): `insert into runs (app_id, workflow_run_id, run_number, head_sha, head_branch, environment, trigger_source) values (...)` with `nullif($5,'')` for head_branch and `on conflict (app_id, workflow_run_id) do nothing returning id`.
- The deploy path resolves app by `(owner, repo, workflow_name, environment)` (`resolveAppID`, dedupe.go:~130-143); the cockpit trigger already receives `app_id` directly — no resolution needed.
- Double-click idempotency: the deploy path's dedupe is the unique constraint + `do nothing` + `ErrNoRows → duplicate` (dedupe.go:206-217). The trigger path should reuse exactly this shape so a replayed trigger returns the existing run rather than erroring; request-level serialization (202 vs 409) is Decision D1.

### 3. Trigger-time budget gate — engine semantics are the trap

[VERIFIED: api/internal/app/migrations/0004_agents.sql:9-12,19-20,26-28; api/internal/agents/budget.go:1-4,18-26,46-93,123-127]

- Session accumulators: `steps_used int`, `cost_millicents bigint` (0004:25-26); hard stops: `step_ceiling int not null default 0` — **0 = unlimited**, `cost_ceiling_millicents bigint not null default 0` — **0 = unlimited** (0004:27-28, verbatim comments).
- `AppendStep` (budget.go:46-49): session row locked `for update` **in the same transaction as the step insert**; success bumps accumulators; breach flips status. Lock SQL verbatim: `select status, steps_used, cost_millicents, step_ceiling, cost_ceiling_millicents from agent_sessions where id = $1 for update` (budget.go:62-65).
- Hard stops are **PROSPECTIVE** (budget.go:76-78): `if stepCeiling > 0 && stepsUsed+1 > stepCeiling` (:79) and `if costCeiling > 0 && cost+step.CostMillicents > costCeiling` (:89) — the ceiling-0 guard inside the engine means an engine-side zero check can never exist; **the trigger endpoint must reject ceiling-0 as a validation error** (CONTEXT:20; roadmap guardrail "Ceiling-0 rejected at the trigger boundary (engine sentinel semantics unchanged)").
- Breach is terminal and non-retryable: `ErrBudgetExhausted` sentinel (budget.go:18-21), status `budget_exhausted` in the closed status check (0004:19-20), `markBudgetExhausted` stamps `finished_at` and appends to `error_note` (budget.go:123-127). River `MaxAttempts: 1` everywhere on this path.
- `ErrTerminal` (budget.go:23-26) covers appends against completed/failed/killed/budget_exhausted sessions.
- Gate query (CONTEXT decision 1): `sum(agent_sessions.cost_millicents)` joined via runs for the app, **current UTC day** — the *day* scoping and the cap floor (`E2AI_DEFAULT_BUDGET_CAP_MILLICENTS`) are cockpit-domain logic; no stored aggregate exists (verified: no budget aggregate table in 0001-0009).
- Trigger-minted sessions always carry explicit positive ceilings: `step_ceiling = len(plan.Steps)`, cost ceiling from app cap (06-CONTEXT.md:21 — P1).

### 4. Global pause — single row, edit rung

[VERIFIED: 06-CONTEXT.md:13; permission ladder below]

- `cockpit_state` single row (skeleton above); pause/unpause is a **write** → `RunEditPermission` (`admin.e2ai.edit` — CONTEXT:13 explicitly assigns pause to the edit rung, not admin) and an audited mutation (row in `cockpit_audit`).
- Blocked surfaces per CONTEXT: manual triggers + webhook simulate. **Open question D4:** whether "the arm accepts no new runs, full stop" also gates the `/github/webhook` path and reconcile/sweep jobs, or only the two cockpit-initiated surfaces. In-flight runs are untouched either way (report-only).
- Surfacing: KPI header + banner read the row (read path can be view-rung or public-within-module).

### 5. Permission ladder + identity (httpx)

[VERIFIED: api/internal/platform/httpx/auth.go:28-45,35-63,114-127; api/internal/agents/http.go:1-8,304-341; api/internal/approvals/http.go:250-273]

- Ladder verbatim (auth.go:32-36):
  ```go
  RunViewPermission  Permission = "admin.e2ai.view"
  RunEditPermission  Permission = "admin.e2ai.edit"
  RunAdminPermission Permission = "admin.e2ai.admin"
  ```
- Identity contract: BFF terminates the session and forwards `X-User`/`X-Permissions`; middleware reconstructs `RequestIdentity` (auth.go:1-6,35-63). No BFF identity → Okta bearer fallback; dev fallback user `"you"` mirrors incidents-admin `DevFallbackUser`.
- `HasPermission` = map lookup on comma-parsed `X-Permissions` (auth.go:114-127).
- Attribution precedent (approvals/http.go:250-273): `resolvedUser(r)` = context identity else `httpx.UserOf(r)`; placeholder identities are refused (400) before any write — the dev-fallback `"you"` cannot perform attributed actions. `writeWire`/`writeErr` JSON helpers, `gate(w, r, edit bool)` (view reads / edit writes) are the surface pattern to copy for cockpit endpoints.
- Route wiring precedent (main.go:336-410): method-prefixed patterns, `if surface != nil` mounts, identity switch returning 401/403 (`GET /api/runs/{id}/plan-resolution` at :380, `POST /api/runs/{id}/derive-draft` at :405 are the inline-handler examples).
- Runner service-token path (`Authorization: Bearer $E2AI_RUNNER_SERVICE_TOKEN`, agents/http.go:1-8) is **not** needed for cockpit endpoints — they are admin-surface only.
- Web manifest today declares only `"permissions": { "view": ["admin.e2ai.view"] }` (admin.module.json:161-166) — Phase 6's edit-rung UI needs the edit rung granted via BFF scope wiring; **staging scopes were not read this session** (`config/admin-bff.staging.yaml` unopened) — planner should checkpoint the staging scope grant (see Open Questions).

### 6. Status machine — where manual runs enter

[VERIFIED: api/internal/runs/state.go:25-45; api/internal/runs/queries_test.go:108-141]

```go
StatusQueued / StatusWaitingRollout / StatusProvisioning / StatusExecuting
/ StatusReporting / StatusCompleted          // state.go:26-31
validTransitions = map[Status][]Status{       // state.go:39-45
  StatusQueued:         {StatusWaitingRollout, StatusFailed},
  StatusWaitingRollout: {StatusProvisioning, StatusFailed},
  StatusProvisioning:   {StatusExecuting, StatusFailed},
  StatusExecuting:      {StatusReporting, StatusFailed},
  StatusReporting:      {StatusCompleted, StatusFailed},
  StatusCompleted:      nil,                  // failed→terminal too
}
```

- Deploy path: `queued → waiting-rollout` (wait-rollout gate job polls health, then) `→ provisioning`, then enqueues `e2ai:run:execute`. Queued→executing is **invalid** (asserted, queries_test.go:130-134).
- Manual runs skip the rollout gate (no GitHub deployment to wait for): insert at **`waiting-rollout`** and enqueue `suite.ExecuteArgs` directly — `waiting-rollout → provisioning` is the legal entry the executor then walks. (`[ASSUMED]` on the executor's precise first act — the Work body was not fully read; the plan must confirm the executor transitions `provisioning` itself or the trigger enqueues after a self-transition. The transition graph above is fully verified.)
- `finished_at` stamps only on terminal entry (queries_test.go:172 comment) — feed duration math should use it, not a computed column.

### 7. Webhook simulate — reuse the real ingest seam

[VERIFIED: api/internal/ingest/http.go:26-31,58-70,274-323; ingest/dedupe.go:8-19; ingest_test.go:79-82]

- The handler resolves every app's `webhook_secret_ref` at boot into an in-memory per-app map (`WithSecretResolver`, http.go:~58-70); resolution failure degrades that app to the shared secret with a warning, never a boot failure. **Simulate reuses this map — secrets never leave the api process** (06-CONTEXT.md:22).
- HMAC flow to mirror in tests: `mac := hmac.New(sha256.New, []byte(secret)); mac.Write(body); "sha256=" + hex...` (ingest_test.go:79-82).
- Body discipline: `maxWebhookBody = 5 << 20` (5 MiB, http.go:~28-30); only `push`/`workflow_run` events accepted (`acceptedEvent` const); unknown workflows/deliveries go to triage (`RecordUnknownDelivery`, http.go:~274-310) — simulate should classify its outcomes the same way the real webhook does.
- Transactional seam (`ingest/dedupe.go` package comment, :8-13): delivery record + run row + wait-rollout enqueue commit atomically — "redeliveries and sweep replays are idempotent by construction." Simulate must call the same funnel (`ingest.HandleEvent` per CONTEXT:22) so outcomes (`ResultAccepted`/duplicate/unknown) and the created-run link come back in one response.
- Simulate endpoint contract (CONTEXT:22): server-side render of a `workflow_run` payload + HMAC sign + real ingest → `{outcome, run_id?}`; blocked when paused. The simulate bad-deploy case renders `conclusion: failure` (test payload builders show `event`/`conclusion` fields, ingest_test.go:~60-72).

### 8. Command Center feed + KPIs — all sources exist

[VERIFIED: api/internal/trends/trends.go:~107; api/internal/agents/http.go:337-341; 0008_alerts.sql; report/page.go:50-54,~235]

- `HistoryRecent(ctx, pool, appID pgtype.UUID, window int) (*History, error)` — window-scoped verdict totals (pass/fail/flaky; flaky counts as failing). **Window-scoped, not day-scoped** (CONTEXT:15) → cockpit mints its own UTC-day KPI queries; do not bend trends.
- Per-run cost: `RunCosts(ctx, pool, runID)` (trends.go:~107) + wire shape `RunCost` via `GET /api/trends/cost/{runId}` (web/src/lib/api.ts).
- Stuck-session flag SQL precedent (agents/http.go:337-341, verbatim expression):
  ```sql
  select id, run_id, role, status,
         status in ('claimed', 'running')
           and coalesce(heartbeat_at, created_at) < now() - interval '90 seconds' as stuck
  from agent_sessions where run_id = $1
  ```
- Feed sources (CONTEXT:15): `runs` (runs/verdicts/cost via sessions), `agent_sessions` (claim/kill/budget_exhausted events; status vocab 0004:19-20), `alert_events` (0008: `kind in ('flake_threshold','flip_rate')`, `payload jsonb`, `acknowledged bool`, index `(app_id, created_at desc)`).
- Approvals badge: reuse `GET /api/approvals` (`getApprovalsQueue` in web/src/lib/api.ts; ApprovalsQueue wire shape locked to types.ts per approvals/http.go:15-26).
- Wire casing: API upper-cases trigger_source on the wire (`TriggerSource string json:"trigger_source"` report/page.go:51; wire values `WEBHOOK`/`RECONCILE` per types.ts:42; DB stores lowercase `webhook`/`reconcile` — 0001:43). → Wire value for manual runs is `MANUAL`.

### 9. Web substrate (LOCKED files — extend only)

[VERIFIED: web/src/lib/types.ts:42; web/src/lib/runStatus.ts; web/src/lib/usePolling.ts; web/src/app/routes.tsx; web/admin.module.json]

- **`web/src/lib/types.ts:42` verbatim: `trigger_source: 'WEBHOOK' | 'RECONCILE'`** — the union is **already missing `SWEEP`** (0007 widened the DB in v1.0; the web union was never extended — pre-existing drift, harmless because sweep rows merely fail narrowing). Phase 6 must extend it to `'WEBHOOK' | 'RECONCILE' | 'SWEEP' | 'MANUAL'`. `types.ts` is LOCKED: extend the union, never rewrite the file.
- `TERMINAL_STATUS = { COMPLETED: true, FAILED: true }` (runStatus.ts) — polling stops on terminal; feed rows reuse `RunStatus` vocabulary.
- `usePolling(loader, intervalMs, enabled, keepGoing?)`: stops on `document.hidden`, resumes on visible, latest payload wins, errors keep previous payload, `keepGoing` callback for terminal/stop conditions — the feed/KPI/pause-banner loaders all follow this; no websockets anywhere.
- Routes (routes.tsx): `index` → `RunsListPage` today; Command Center becomes the index (COCK-01 "landing at the module root") — swap index element to `CommandCenterPage`, keep `/runs` explicit. Page convention: `web/src/features/<domain>/<Page>.tsx`.
- Manifest (`web/admin.module.json`): id `@admin/e2ai`, one nav section `operations`, commands with `{id,title,route,icon,section,keywords}`; permissions view-only today; `navigationOrder: 50`. Add a `cockpit` command (route `/e2ai`, icon, section `operations`) + i18n in all four locales (en-US/pt-BR/es/ro are mandatory in every user-facing string — manifest + UI). Validation: `bun run validate:manifest`.
- Dev shell: `main.tsx` supports `bff` mode (`apiUrl="/api"`, `bffBaseUrl`) and in-memory mock auth (`createInMemoryAuthClient`, `createMockUsersFromRoles`) — cockpit dev flows need a mock user carrying `admin.e2ai.edit`.
- api.ts conventions: `getJson`/`postJson` over `authenticatedFetch` (web/src/shared/authenticatedFetch.ts); every new cockpit call pairs with a locked wire DTO added to types.ts.

## Approach Options / Decisions Needed

| # | Decision | Options | Recommendation | Confidence |
|---|----------|---------|----------------|------------|
| D1 | Concurrent-trigger serialization (one 202, one 409) | (a) River `UniqueOpts` on the ExecuteArgs enqueue (precedent: canary.go:155-158, reportOpts execute.go:67-73 — unique across scheduled/pending/available/running); (b) insert-as-gate partial unique index on active manual runs per app | (a) — matches shipped unique-by-state pattern; run-row `on conflict do nothing` already absorbs replays | HIGH |
| D2 | `run_number` for manual runs | (a) 0 sentinel; (b) per-app `max(run_number)+1` | (a) — column is `not null int` with no uniqueness; 0 renders as "manual" in feed; (b) implies a fake GitHub numbering scheme | HIGH |
| D3 | Synthetic `workflow_run_id` polarity | (a) dedicated sequence with negative values (GitHub ids are positive; collision impossible); (b) high-offset positive sequence | (a) — fail-closed against any future GitHub id collision; CONTEXT says "dedicated sequence" without polarity | HIGH |
| D4 | Pause enforcement blast radius | (a) cockpit surfaces only (trigger + simulate — the explicit CONTEXT enumeration); (b) every run-creation seam incl. `/github/webhook`, reconcile, sweep ("full stop" parenthetical) | Needs **user confirmation** — the two readings produce different behavior for deploys landing during pause. Recommend (b) per the parenthetical, with reconcile/sweep enqueues checking the flag before handoff | MEDIUM — flag to discuss-phase |
| D5 | Command Center routing | (a) index route swap; (b) `/cockpit` + redirect | (a) — COCK-01 says landing at module root; RunsListPage moves to its existing `/runs` path | HIGH |
| D6 | Feed endpoint shape | (a) single UNION endpoint `/api/cockpit/feed` with keyset pagination `(created_at desc, id)`; (b) per-source endpoints merged client-side | (a) — matches runs-list limit conventions (defaultListLimit 50 / max 200, runs/queries.go:70-73); one poll, one payload | HIGH |
| D7 | Audit table granularity | one row per cockpit mutation (trigger/simulate/pause/unpause) with closed `action` check vocabulary, extensible for P7-P9 | As stated — mirrors 0008's closed-kind pattern | HIGH |

## Common Pitfalls (P1–P6 × Phase 6)

Grounded in `.planning/research/PITFALLS.md` (amplification table: cockpit controls × v1.0 agent-domain risks); phase-6 treatment verified in code this session.

- **P1 — Ceiling-0 mints unbounded sessions.** Engine reads `0 = unlimited` (0004:27-28, budget.go:79,89). A trigger path that copies app defaults carelessly can mint a session with no budget. *Treatment:* trigger endpoint validates positive ceilings before insert (`step_ceiling = len(plan.Steps) > 0`, cost ceiling from app cap or env floor); ceiling-0 → validation error at the boundary; engine untouched. Test: ceiling-0 trigger request → 4xx, engine sentinel semantics unchanged.
- **P2 — Budget gate checked at the wrong moment.** Gate must run inside the trigger transaction against the UTC-day aggregate (CONTEXT:12) — a pre-flight UI-only check races concurrent triggers. *Treatment:* 409 with remaining-budget reason computed server-side in the same tx; UI pre-flight is display-only. Warning sign: two triggers both pass while spend ≈ cap.
- **P3 — Double-click double-spawn.** Two rapid POSTs must yield one 202 + one 409. *Treatment:* D1(a) UniqueOpts; run-row `on conflict do nothing` makes replays return the original run. Regression: concurrent-trigger test asserting exactly one 202.
- **P4 — Audit misattribution (M4).** Actor must come from BFF identity (`resolvedUser`, approvals/http.go:267-273), never from request body; placeholder/dev-fallback identities refused 400 before write (approvals precedent). *Treatment:* cockpit_audit rows carry `actor` from context only; audit insert in the same tx as the mutation.
- **P5 — Secrets leaking through simulate.** The simulate feature renders payloads + signs HMAC server-side; any echo of the resolved secret, or client-side signing, is a critical leak. *Treatment:* simulate handler uses `WithSecretResolver`'s in-memory map server-side only; response contains outcome + run link only; error text follows secrets.go's redaction discipline (never include secret material).
- **P6 — Report-only invariant erosion.** Trigger/simulate/pause actuate the arm (River jobs, run rows), never the app under test — no deployments, no workflow dispatches, no plan/corpus rewrites (0009 report-only posture is the precedent). *Treatment:* code review gate: cockpit phase touches only `runs`, `webhook_events`, `agent_sessions`, `cockpit_*` tables and River enqueues; any write to `plan_approvals`/corpus/registered_apps content is out of scope (cap column DDL excepted, locked in CONTEXT).
- **Web union drift (new, phase-specific).** `types.ts:42` already lags the DB (`SWEEP` missing). Adding `MANUAL` without `SWEEP` perpetuates the drift. *Treatment:* extend with both, in the LOCKED file, by union extension only.
- **Pause ambiguity (D4).** Shipping pause that silently lets webhook runs through (or silently blocks real deploys) breaks operator trust in the KPI header. *Treatment:* resolve D4 with the user before planning the gate placement; surface paused state in feed responses so the UI can't desync.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Run dedupe/idempotency | custom replay logic | `insertRun` on-conflict pattern + River `UniqueOpts` | Atomic-by-construction (dedupe.go package comment) |
| Budget accounting | new accumulator table | `agent_sessions` columns + `AppendStep` tx | Engine is transactional, terminal, non-retryable |
| HMAC signing | custom signature scheme | `crypto/hmac` sha256, exact GitHub scheme | Real ingest verifies the real format (ingest_test.go:79-82) |
| Identity/permissions | new middleware | httpx ladder + `gate()`/`resolvedUser` | One contract across all surfaces |
| Feed events | `feed_events` write path | UNION read over runs/agent_sessions/alert_events | CONTEXT decision 4 forbids write amplification |
| Day KPIs | bending trends.HistoryRecent | cockpit-domain UTC-day queries | trends is window-scoped by design (CONTEXT:15) |

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go `testing` (stdlib) + httptest; no web unit framework exists |
| Config file | none — convention-based (`*_test.go` beside code) |
| Quick run command | `go test ./api/internal/...` (DB-less; DB tests self-skip) |
| Full suite command | `go test ./...` |
| Web gate | `cd web && bun run build` (`tsc && vite build`, package.json:7-8) + `bun run validate:manifest` |

### Harness facts
[VERIFIED: ingest_test.go:129-131,361-371; e2e_test.go:245-249]
- DB-backed tests require `E2AI_TEST_DATABASE_URL` and **skip silently when unset** (ingest_test.go:362-364) — the pipeline's DB tests (e2e_test.go) are no-ops without it.
- Fakes: `fakeStore`/`fakeEnqueuer` + `newTestHandler(store, queue)` build the ingest handler without River/DB (ingest_test.go:129-131) — trigger/simulate unit tests follow this.
- e2e tests assert pipeline outcomes by direct SQL (`select id from runs where workflow_run_id = 88001`, e2e_test.go:245-249) and by trigger_source/delivery_id (ingest_test.go:540-544) — manual-trigger e2e assertions should follow this exact shape.
- `seedApp`/`seedRun` helpers live in runs/queries_test.go for status-machine tests.

### Phase Requirements → Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| BUDG-03 | ceiling-0 refused at trigger boundary; engine semantics untouched | unit | `go test ./api/internal/cockpit/ -run TestTriggerCeilingZero` | ❌ Wave 0 |
| BUDG-03 | UTC-day aggregate vs cap → 409 with remaining reason | unit (sqlmock-free, DB-skippable) + DB e2e | `go test ./api/internal/cockpit/`; `E2AI_TEST_DATABASE_URL=... go test ./api/internal/cockpit/ -run E2E` | ❌ Wave 0 |
| TRIG-01..05 | trigger inserts run at waiting-rollout, trigger_source='manual', synthetic ids; enqueues ExecuteArgs (runner queue) | unit + DB e2e | `go test ./api/internal/cockpit/ ./api/internal/ingest/` | ❌ Wave 0 |
| TRIG (fail-closed) | `trigger_source='manual'` violates 0007 check pre-migration, passes after | DB e2e (constraint-level) | `E2AI_TEST_DATABASE_URL=... go test ./api/internal/app/migrations/ -run TestManualTriggerSource` | ❌ Wave 0 |
| TRIG-05 | concurrent triggers: exactly one 202, one 409 | unit (UniqueOpts shape, canary_test.go:92-95 pattern) | `go test ./api/internal/cockpit/ -run TestConcurrentTrigger` | ❌ Wave 0 |
| COCK-04 | pause blocks trigger + simulate; unpause audited; edit-rung enforcement (401/403) | unit | `go test ./api/internal/cockpit/ -run TestPause` | ❌ Wave 0 |
| ACTN-01 | simulate HMAC-signs with resolved secret, drives real ingest, returns outcome + run link; blocked when paused | unit (sign() helper) | `go test ./api/internal/cockpit/ -run TestSimulate` | ❌ Wave 0 |
| AUDT-01 | audit row written in same tx as mutation; actor from identity; placeholder refused | unit | `go test ./api/internal/cockpit/ -run TestAudit` | ❌ Wave 0 |
| COCK-02 | feed UNION endpoint returns runs/sessions/alerts, keyset-paginated | DB e2e | `E2AI_TEST_DATABASE_URL=... go test ./api/internal/cockpit/ -run TestFeed` | ❌ Wave 0 |
| COCK-01/02 (web) | Command Center compiles; types.ts union extended not rewritten | build gate | `cd web && bun run build && bun run validate:manifest` | existing scripts |

### Sampling Rate
- **Per task commit:** `go test ./api/internal/...` + targeted new package test
- **Per wave merge:** `go test ./...` + `cd web && bun run build`
- **Phase gate:** full suite green (+ DB-backed e2e with `E2AI_TEST_DATABASE_URL` when a test DB is reachable) before `/gsd-verify-work`

### Wave 0 Gaps
- [ ] `api/internal/cockpit/` package with surface + domain + tests (endpoint, gate, pause, simulate, audit, feed) — all Phase 6 Go tests land here
- [ ] DB e2e for trigger/simulate/pause following ingest e2e harness (`E2AI_TEST_DATABASE_URL` bootstrap, e2e_test.go:245 pattern)
- [ ] web: no test framework — rely on `tsc` build gate + manifest validation (do not introduce one this phase)

## Environment Availability

| Dependency | Required By | Available | Version | Fallback |
|------------|------------|-----------|---------|----------|
| Go | api build/tests | ✓ | 1.26.6 darwin/arm64 | — |
| Bun | web build, manifest validation | ✓ | 1.4.2 | — |
| Postgres | DB e2e tests (`E2AI_TEST_DATABASE_URL`) | ✗ at probe (:5432 closed) | — | Tilt environment provides it for dev; DB tests self-skip when unset — plan must not *require* DB e2e to pass in environments without it, but the trigger-path tests are DB-dependent |
| 1Password CLI (`op`) | webhook secret resolution at boot (secrets.go) | ✓ (guarded confirmation) | — | Without it, apps degrade to shared secret with warning (http.go:~58-70) — ingest still boots |
| Docker/Tilt | local dev stack (BFF routes to host.docker.internal:8082) | not probed (guardrail-gated) | — | `admin-bff.yaml` documents the topology; planner may assume Tilt dev flow per STACK.md |

**Missing dependencies with no fallback:** none blocking implementation; DB e2e requires a reachable Postgres (`E2AI_TEST_DATABASE_URL`) — unit-level coverage must be DB-less so the suite stays green without it.

## Runtime State Inventory (migration phases only — condensed)

Phase 6 is an **additive schema migration**, not a rename — full inventory not triggered. The only runtime-state considerations:

| Category | Items Found | Action Required |
|----------|-------------|------------------|
| Stored data | Pre-0010 runs rows carry only webhook/reconcile/sweep sources — no backfill needed; `cockpit_state` seeded by migration | none |
| Live service config | BFF Okta scope granting `admin.e2ai.edit` (staging yaml not read this session) | human/config step at deploy; flag in plan |
| OS-registered state | None — verified (no renames involved) | none |
| Secrets/env vars | New env `E2AI_DEFAULT_BUDGET_CAP_MILLICENTS` (floor); existing `op://` refs unchanged | code reads env; secret refs untouched |
| Build artifacts | None | none |

## Security Domain

`security_enforcement: true`, `security_asvs_level: 1`, `block_on: high` (.planning/config.json).

| ASVS Category | Applies | Standard Control |
|---------------|---------|-----------------|
| V2 Authentication | delegated | BFF Okta wall (out of module scope; dev fallback only for un-proxied local) |
| V3 Session Management | delegated | BFF-owned |
| V4 Access Control | **yes** | httpx rungs; every cockpit write behind `RunEditPermission`; server re-checks even though frontend hides |
| V5 Input Validation | **yes** | strict request DTO (`app_id, head_sha, head_branch?, environment, reason?`), UUID parse helpers, body caps (maxWebhookBody precedent) |
| V6 Cryptography | **yes** | HMAC-SHA256 stdlib only; secrets rendered by `op` CLI, never hand-rolled crypto |
| V7 Error/Logging | yes | `mapDomainError` mapping (404/409/500); secrets never in logs (secrets.go redaction) |
| V8 Data Protection | **yes** | resolved secrets stay in api memory; simulate response carries outcome only |
| V14 Config | yes | cap floor env var validated at boot; unset = uncapped-explicit state (not an error) |

| Threat Pattern | STRIDE | Mitigation |
|----------------|--------|------------|
| Client-supplied actor in audit rows | Repudiation | `resolvedUser` server-side; placeholder 400 (approvals/http.go:267-273) |
| Secret exfiltration via simulate | Information disclosure | server-side sign only; outcome-shaped response |
| Concurrent-trigger double-spend | Tampering | UniqueOpts serialization + tx gate (D1/P3) |
| Permission bypass via direct API call | Elevation | gate() on every write; dev-fallback identity cannot pass attribution refusal |
| Audit-row tampering | Repudiation | audit insert in same tx, no update/delete path |

## Assumptions Log

| # | Claim | Section | Risk if Wrong |
|---|-------|---------|---------------|
| A1 | Executor's first act on a manual run is transitioning `waiting-rollout → provisioning` (or the trigger performs it) | Key Facts 6 | Plan would mis-sequence the enqueue; caught immediately by status-machine tests |
| A2 | Pause enforcement scope (D4) resolves to "all run creation" per the "full stop" parenthetical | Approach Options | Gate placement differs; user confirm before planning |
| A3 | Docker/Tilt present locally (STACK.md's Tiltfile-based dev flow) | Environment Availability | Dev-only impact; tests are Tilt-independent |

## Open Questions

1. **Pause blast radius (D4)** — decision 2 enumerates manual-trigger + simulate explicitly but says "the arm accepts no new runs, full stop." Does a paused arm refuse `/github/webhook` deliveries (and reconcile/sweep hand-offs), or only cockpit-initiated runs? → **user confirm** (recommend: block all creation, check flag at each insertion seam).
2. **Staging BFF scope wiring** — `config/admin-bff.staging.yaml` was not read; who grants `admin.e2ai.edit` (which Okta groups/scopes)? → planner adds a `checkpoint:human-verify` for scope wiring; local dev works via mock users.
3. **Executor first transition (A1)** — confirm from `suite/execute.go` Work body during planning (file was structurally read; body elided).

## Sources

### Primary (HIGH confidence — read this session)
- `api/internal/ingest/{dedupe.go,http.go,rollout.go,secrets.go}` — ingest spine, HMAC flow, secret map
- `api/internal/app/migrations/{0001,0004,0006,0007,0008,0009}` — schema, constraints, ceiling columns, migration style
- `api/internal/agents/{budget.go,http.go}` — ceiling semantics, stuck flag, kill switch, surface pattern
- `api/internal/platform/httpx/auth.go` — permission ladder + identity contract
- `api/internal/approvals/http.go` — attribution + gate pattern
- `api/internal/suite/{execute.go,canary.go}` — ExecuteArgs, runner-queue opts, plan-resolution pin gate
- `api/internal/runs/{state.go,queries_test.go}` — status machine verbatim
- `api/internal/trends/trends.go`, `api/internal/report/page.go` — cost/window aggregates, wire casing
- `api/cmd/e2ai-api/main.go` — route wiring, worker wiring
- `web/src/lib/{types.ts,api.ts,usePolling.ts,runStatus.ts}`, `web/src/app/routes.tsx`, `web/src/main.tsx`, `web/admin.module.json` — locked web substrate
- `config/admin-bff.yaml` — local dev BFF shape
- `.planning/phases/06-cockpit-trigger-command-center/06-CONTEXT.md`, `.planning/REQUIREMENTS.md` (v1.1), `.planning/config.json`

### Secondary (HIGH confidence — milestone research, cited not restated)
- `.planning/research/{STACK,ARCHITECTURE,PITFALLS,SUMMARY}.md`

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — zero new deps; all mechanisms read in shipped code
- Architecture/seams: HIGH — file:line verified throughout; two body-level details flagged (A1, staging scopes)
- Pitfalls: HIGH — P1-P6 grounded in shipped code reads; D4 is a decision, not a gap

**Research date:** 2026-09-23
**Valid until:** 2026-10-23 (stable — zero-dependency phase, repo-grounded)
