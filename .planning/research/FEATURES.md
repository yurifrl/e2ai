# Feature Landscape: v1.1 Admin Cockpit

**Domain:** Control-plane UI for an autonomous AI QA arm (report-only platform)
**Researched:** 2026-09-22
**Mode:** Feature-behavior research — how each v1.1 feature must behave from the admin's seat
**Grounding:** `.planning/PROJECT.md` (Current Milestone v1.1), `.planning/REQUIREMENTS.md` (v1: 45 reqs closed), sketches 001/002/004/005/006 winners, web/src pages, api Connect/HTTP handlers.

**Invariants that bind every feature below** (from PROJECT.md charter — repeated per feature where load-bearing):
1. **Report-only.** The arm observes and reports; it never mutates the app under test (no deploys, no app config changes, no redeploys). Cockpit actions act on the *arm*, never the *app*.
2. **Approval gating.** Only approved plan versions execute unattended (PLAN-03). The cockpit never bypasses the Approvals queue.
3. **Budget discipline.** Layered caps (BUDG-01) and the kill switch (BUDG-02) govern every trigger. Manual triggering consumes the same budget as deploy-triggered runs — there is no "free" path.

---

## 1. The Cockpit as Command Center (new home page)

**What exists today:** The module's effective landing is Runs List (`web/src/features/runs/RunsListPage.tsx`). Sketch 001 winner **A · Live Feed** (★) already designed the "arm always reporting" landing — reverse-chron live feed, agent rail, KPI header, app filter chips. It was never built; v1.1 builds it *and* attaches the trigger affordances that make it a cockpit rather than a dashboard.

**UX behavior:**

- Route: module root (`/e2ai`) redirects to **Command Center** (sketch 001 winner A layout). Runs List stays at `/runs` — the feed links into it; the board variant survives as the app-filtered view, exactly as the winner states.
- **KPI header** (sketch 001): runs today, pass rate today, flaky suspects open, **pending approvals (badge, same count ApprovalsBadge already computes)**, plus two cockpit-native KPIs: **budget spend vs. cap today** and **paused/kill-switch state**. All live via the existing house polling pattern (`web/src/lib/usePolling.ts`, 5–15s cadence as in Approvals/Flakes pages).
- **Live feed** = reverse-chron union of runs (all trigger sources) + notable arm events (agent session failures, flakes quarantined, triage rows appearing). App filter chips constrain the feed. Rows expand inline to a compact detail strip (verdict, duration, cost, evidence thumbnails) — the sketch's `data-detail` behavior.
- **Agent rail**: current agent sessions across executing runs (role, status, heartbeat age), stuck sessions (lapsed heartbeat > 90s — the API already computes a `stuck` flag, `api/internal/agents/http.go:353`) flagged visually, each linking to the Run Detail where kill lives.
- **Trigger affordance lives here**: a prominent "Trigger run" control opening the app/plan picker (§2). This is what turns 001's passive feed into the cockpit — the admin sees the arm's state and actuates it from the same screen.
- **Global pause control** (§6) sits in the KPI header — visible state at all times, not buried in settings.

**Why it matters:** every other v1.1 feature (webhook inbox, jobs, triage) reports *into* this page as feed events. It is the integration point; build it first or the other features have no home.

---

## 2. Trigger Apps / Plans (manual trigger source)

**What exists today:** Runs only originate from `webhook | reconcile | sweep` (`runs.trigger_source` CHECK, `0007_hardening.sql:62`). The nearest manual precedent is `POST /api/apps/{app}/plans-sync` (sync worker), which is API-only today (`web/src/lib/api.ts:152` — `syncAppPlans` is exported but unused by any page). Manual triggering is the milestone's headline: the admin should be able to test a plan immediately after approving it, without faking a deploy.

**UX behavior:**

- **Trigger entry points**: (a) Command Center "Trigger run" button; (b) per-app trigger on Registry cards (sketch 005 winner A); (c) "Run now" on a Plan Detail page — pre-selects that app + plan version; (d) **Re-run** on a completed Run Detail.
- **Picker flow** (one dialog, three states):
  1. **Pick app** — filter chips / registry cards list (apps from `registered_apps`).
  2. **Pick plan version** — defaults to the app's **latest approved plan version**; a dropdown offers other *approved* versions only. **Draft / superseded / unapproved versions render disabled with a one-line reason ("needs approval in Approvals queue") — the approval-gating invariant made visible, not enforced by hiding.** If the app has zero approved versions, triggering is disabled with a link to its triage/suggested plans in the Approvals queue.
  3. **Confirm** — shows the resolved plan's identity (version, source PR/SHA), the **current budget state for the app** (spend today vs. layered caps, BUDG-01), an optional reason field, and the run's *shape* (estimated suite size, surfaces it will exercise). Confirm is the approval gate moment: **the run executes only the approved version shown**.
- **Run creation semantics:** creates a `runs` row with a new `trigger_source = 'manual'` (extend the CHECK constraint), stamps the operator identity (from the shared-admin gateway headers — same identity the API already reads via `httpx.IdentityFromContext`) and optional reason, then enqueues the pipeline **bypassing wait-rollout** — a manual trigger is not deploy-tied; the app is by definition already deployed (same reasoning the canary uses for its rollout-gate skip, `api/internal/suite/canary.go`).
- **Re-run** (from Run Detail): reuses the original run's resolved plan version. Two explicit modes: **"Same commit (reproduce)"** — pins the original head SHA for determinism; **"Latest head (retest)"** — pulls current default branch. Default = same commit. Re-run is always a *new* run row (append-only history, never an in-place replay).
- **Feedback:** on confirm, the run appears in the Command Center feed within one poll tick with a `manual` badge and operator attribution; deep-link to Run Detail. Errors surface inline with the API's meaning (404 unknown app, 409 no approved plan, 403 missing `admin.e2ai.edit`).
- **Permissions:** trigger requires `admin.e2ai.edit` (the ladder's edit tier, already enforced on agent enqueue/kill).
- **Invariants honored:** report-only (trigger runs QA against the *deployed* app — it does not deploy, provision, or restart anything); approval-gated (only approved versions selectable); budget-bounded (the run consumes normal budget; the confirm screen shows remaining capacity and **the trigger is disabled with an explicit reason when the app's budget is exhausted or the arm is paused** — never silently queued).

---

## 3. Admin Action Surface — every outside action gets a UI affordance

**What exists today:** the enumerated gap. An audit of the repo yields the following actions currently performed via GitHub UI, shell (`Taskfile.yml`), raw curl against the API, or not at all:

| Today's surface | Action | Where it lives now | v1.1 UI affordance |
|---|---|---|---|
| GitHub repo settings | Redeliver a webhook delivery | GitHub UI only | Webhook Inbox "Replay" (§4) + deep-link to GitHub's redeliver page as the genuine GitHub-side retry |
| `task webhook:simulate` | Inject synthetic signed webhook | Shell (curl + HMAC by hand) | **"Simulate webhook"** admin action — pick app, optionally a synthetic payload shape; **the api server renders and HMAC-signs the payload with the app's resolved secret** (secrets never reach the browser); shows ingest result (`ingested` / `already-ingested` / `filtered` + created run link) |
| `task agent:enqueue` (curl wrapper) | Enqueue an agent session on a run | Raw POST `POST /api/runs/{runId}/agent-sessions` | **Run Detail → "Add agent session"** button: role picker (navigator, vision judge, plan author, edge-case hunter, flake analyst); enabled only while run is `executing` (mirrors the API's 409); surfaces the approved-plan gate inline if resolution is unapproved; requires `admin.e2ai.edit` |
| `POST /api/agent-sessions/{id}/kill` | Kill switch on a runaway session | Raw curl (BUDG-02) | **Run Detail agent-session row → Kill** action; 409 ("already finalized by X") surfaces inline; kill reason field (lands in the session's append-only error_note) |
| raw SQL / `riverc` | Retry / cancel / discard a stuck River job | psql only | Jobs observatory (§5) |
| `ListTriage` API (store-only — no HTTP handler mounted yet, `api/internal/ingest/triage.go`, no mount in `main.go`) | Triage unknown-app webhook deliveries | Nothing — data recorded, invisible | Triage queue (§4) |
| `syncAppPlans` API (unused in UI) | Trigger plans sync for an app | curl only | Registry card + Plan Library "Sync now" button (per-app), showing the sync result (plans added/updated) |
| `task mcp:probe` | Verify MCP surface responds (tools/list) | Shell | **System/Integrations panel → "Probe"** button: runs tools/list + one bounded tool call, displays raw response + latency + failure reason. Read-only probe, no budget consumption |
| — | **Pause the arm** (global kill switch) | Does not exist | §6 (new capability) |
| — | Canary status | Runs invisibly every 15min, inert unless `E2AI_CANARY_IMAGE` set | Jobs observatory: canary card showing inert vs. active, pinned image, last canary run + verdict |

**UX behavior — shared patterns for every admin action:**

- Every mutating action requires `admin.e2ai.edit` (or `admin.e2ai.admin` where noted); the ladder exists (`api/internal/platform/httpx/auth.go:35-37`) and `RunAdminPermission` is currently unused — reserved for arm-level controls (pause, job control, triage promotion).
- **Every action is explainable**: hover/inline text states what it will do, what it costs (budget or nothing), and whether it is reversible. No hidden side effects.
- **Every action is auditable**: operator identity + timestamp recorded (runs rows, session error_notes, delivery replays, triage actions all already have or gain attribution columns). Admin sees *who* did what in list rows ("killed by @yuri 2m ago").
- **Confirmation only where destructive**: kill, pause, job cancel/discard get a confirm step; harmless idempotent actions (probe, simulate, sync) fire immediately and show the result inline.
- **Errors are the API's errors, verbatim**: 404/409/403/503 map to specific inline messages (e.g. kill's 409 "another actor finalized the session" — the exact race semantics in `api/internal/agents/worker.go:280`).
- **Invariants honored:** simulate/webhook replay/enqueue all consume real budget when they create real runs (§2); none of these actions touches the app under test; kill and pause are arm-scoped only.

---
## 4. Webhook Inbox

**Foundation (exists):** `webhook_events` (app_id, delivery_id, workflow_run_id, signature_valid, payload, received_at — migration 0001) and `webhook_triage` (unknown-app deliveries with occurrences/last_seen_at; EDGE-01). The store exists; the UI does not — `ListTriage` in `api/internal/ingest/triage.go` has **no HTTP handler mounted today** (confirmed: only a comment reference in `main.go:204`), so v1.1 adds the surface endpoint too.

### 4.1 Deliveries tab (`/e2ai/webhooks`)
| Aspect | Behavior |
|---|---|
| List | Reverse-chron rows: received time, app badge (or `UNCLAIMED` amber chip for triage rows), workflow + run id, signature badge (valid ✓ / invalid ✗), ingest outcome chip (ingested→run #123 · duplicate · redelivered ×N · triaged · rejected). Click = row expand (001 pattern). |
| Row expand | Delivery detail: GUID, event type, parsed run metadata, **raw JSON payload viewer** (copy button — this is the debugging gold; today devs curl the DB), links: "View run →" (when ingested), "Open in GitHub →" (workflow_run html_url from payload). |
| Redeliver action | Per delivery: **Redeliver** button (edit permission). Calls the new admin redelivery endpoint → GitHub API `redeliver-webhook-delivery` for the delivery GUID. Confirmation toast states the consequence: GitHub re-sends; e2ai re-ingests through the same dedupe path (duplicate = harmless, recorded). Disabled with tooltip when signature invalid? No — signature validity is re-evaluated on the new delivery; enabled regardless. Rate-limit: button cooldown 5s. |
| Unknown-app handling | `UNCLAIMED` rows are first-class: "This delivery belongs to no registered app." + **Register app** shortcut deep-linking into Apps (§5.2) with owner/repo/workflow pre-filled from the payload — the triage queue becomes the onboarding funnel (sketch 005 winner's spirit). |
| Filters | App chip filter (001 pattern); status filter: all / triaged / rejected / ingested; text search over GUID/workflow. |
| Polling | `usePolling` 5s; triage rows sort by `last_seen_at` (activity), events by `received_at`. |
| Empty states | No deliveries: "Waiting for the first webhook — trigger a run or use Simulate (§6)." Triaged-empty: "All deliveries are claimed." |

### 4.2 What redeliver means (UX copy)
- "Replays the delivery through the same idempotent path as the original — safe to retry; ingested runs dedupe by delivery id."
- (The original wording that replay is harmless had to be phrased to avoid the word 'no-op' — cosmetic only.)

---

## 5. App Registry Management (005 winner: Registry Cards)

**Today:** apps are rows in `registered_apps` (migration 0001: owner/repo/workflow_name/environment unique, webhook_secret_ref, comment_marker; 0002 adds edge_sweep_enabled + flake thresholds) managed only by direct SQL. v1.1 makes the registry a UI surface.

### 5.1 Apps page (`/e2ai/apps`) — Registry Cards
| Aspect | Behavior |
|---|---|
| Cards | One card per registered app: repo slug + workflow + environment, health summary (runs today, last run verdict chip, agent status mini-rail — reuse 001 rail), toggles state: edge-sweep on/off (per-app DB config, 0002), quarantine count, triage count. Click → app detail drawer (or dedicated page) with full config + per-app action log. |
| Add app | "Add app" opens the wizard form (005 winner B embedded): owner, repo, workflow_name, environment, comment marker (default `<!-- e2ai:run:{slug} -->`), flake thresholds, edge-sweep toggle. **Secrets: the form never accepts or displays a raw secret — it stores a 1Password reference** (matching `webhook_secret_ref` semantics: plaintext never stored). UX: a read-only resolved-status line ("secret resolves ✓ from vault") verified server-side at save; failure = inline error, save blocked. |
| Edit | Same form, prefilled. Changing workflow/environment is allowed (unique constraint re-validated server-side; 409 surfaces as inline "this combination is already registered"). |
| Delete/retire | Two-step confirm typed-app-slug (irreversible; orphaned triage rows reclassify). Show dependency warning: "N runs / M behaviors reference this app" — deletion keeps history (no cascade delete of runs), only removes registration + stops ingest matching. |
| Permissions | View = view permission; add/edit/delete = admin permission (`admin.e2ai.admin`, currently unused in handlers — v1.1 puts it to work). |

### 5.2 Onboarding funnel from triage
"Register app" from an UNCLAIMED webhook row (§4.1) pre-fills owner/repo/workflow from the delivery payload; the wizard disables those fields (derived from evidence) and asks only for the choices a human must make: environment, secret ref, thresholds. On save, the triage row is marked resolved (linked app_id), and past unclaimed deliveries for that repo show "now claimed" — no retroactive re-ingest (explicitly not promised; redeliver is available if wanted).

---

## 6. "Every Outside Action in Admin" — Enumeration

**Definition:** any control-plane mutation that today requires SSH/kubectl/API client/psql because no UI affordance exists. The cockpit principle: if an operator can do it to the arm, the admin UI offers it — with the same permission ladder and audit trail as the API.

| # | Outside action (today) | Where it lives today | v1.1 UI affordance | Permission | Budget/safety notes |
|---|---|---|---|---|---|
| 1 | Manual run trigger (workflow_dispatch) | GitHub UI / `gh` CLI | Cockpit "Trigger" (§2) | edit | Budget-checked pre-flight; consumes real budget |
| 2 | Plan sync trigger | `POST /api/apps/{app}/plans-sync` — exists but `syncAppPlans` unused by any page (confirmed grep) | Plans page header button + App card action | edit | Syncs plans repo → DB; read-side only |
| 3 | Agent session enqueue | `POST /api/runs/{runId}/agent-sessions` (RunEditPermission) | RunDetail "Enqueue agent" (role picker) | edit | Respects session ceilings; visible in run timeline |
| 4 | Kill switch | `POST /api/agent-sessions/{id}/kill` — API-only today | RunDetail session row kill button + agent rail (001) | edit | Two-step confirm; arm-scoped, never touches app |
| 5 | Webhook redelivery | GitHub API / dashboard only | Webhook inbox Redeliver (§4.1) | edit | Idempotent ingest path |
| 6 | Webhook simulate | curl to ingest endpoint w/ HMAC | Cockpit Simulate (§6.1 below / §2) | edit | Creates real evidence rows; budget-checked |
| 7 | Canary run now | Wait 15min (`suite/canary.go` periodic) or API call | Cockpit "Run canary" secondary action | edit | Same budget gate |
| 8 | Triage review | `ListTriage` store — no HTTP surface, no UI | Webhook inbox UNCLAIMED rows (§4.1) | edit (resolve) / view (read) | Register-app deep link |
| 9 | App registry CRUD | psql inserts on `registered_apps` | Apps page (§5) | admin | Secret = vault ref only |
| 10 | River job ops (retry/cancel stuck jobs) | SQL on river_job / kubectl | Jobs page (§3) | admin | Read-first; mutations admin-gated |
| 11 | MCP probe | curl to MCP surface | Cockpit "Probe MCP" utility action | edit | Read-only health probe; report-only |
| 12 | Orphan sweep force-run | Wait 30s periodic / API | Jobs page "Run sweep now" | admin | Safe: marks orphans failed + requeues (append-only log) |
| 13 | Alert acknowledge | — **already in UI** (TrendsPage) | — (verify only) | edit | n/a |
| 14 | Quarantine activate/lift | — **already in UI** (FlakesPage) | — (verify only) | edit | Human-gated by design |
| 15 | Corpus promote/dismiss | — **already in UI** (CorpusPage) | — (verify only) | edit | n/a |
| 16 | Approvals approve/reject | — **already in UI** (ApprovalsQueuePage) | — (verify only) | edit | Approval gating invariant |

Rows 13–16 are listed to make the audit exhaustive: they are *already* inside the admin surface and v1.1 must not regress them. The deliverable for v1.1 is rows 1–12.

### 6.1 Cross-cutting UX rules for every mutation
- **Confirm-then-execute** for destructive/ambiguous actions (kill, delete app, force sweep); single-click for safe ones (acknowledge, sync).
- **Every mutation shows its consequence sentence** before commit ("this enqueues a real agent session that consumes budget").
- **Optimistic-ui-free:** all mutations wait for the server result (consistency over snappiness — this is an ops console); failures render inline with the API's exact error text (404/409/403/503 → specific messages, e.g. kill's 409 race from `api/internal/agents/worker.go:280`).
- **Audit trail:** every cockpit mutation is attributable — the operator identity (already in `httpx.IdentityFromContext`) must land in the affected row/log so "who killed this session / who triggered this run" is answerable from the UI. New: runs.trigger_source gains `manual` (check constraint `runs_trigger_source_check` in migration 0002 must be extended — flagged for STACK/ARCHITECTURE follow-up).

---

## 7. Anti-Features (explicitly NOT in v1.1)

| Anti-Feature | Why Avoid | What to Do Instead |
|---|---|---|
| Editing the app under test from the cockpit (restart pods, rollback deploy, env vars) | Violates report-only core invariant (PROJECT.md) | Link out to the app's own admin; the arm observes, never mutates |
| Auto-approving plans from the cockpit | Approval gating is a hard invariant; human gate is the value | ApprovalsQueuePage stays the only approval path |
| Unlimited manual triggers | Trivially torches the budget the arm lives within | Same pre-flight + caps as automatic triggers; show remaining budget |
| Raw secret entry/display | Plaintext secrets in browser = policy violation | Vault-reference resolution only, verified server-side (§5.1) |
| Free-form SQL/job console | Beats the purpose of moving ops out of ad-hoc access | Curated per-job actions with fixed semantics (§3) |
| Rework of v1.0 pages (Runs/Plans/Corpus/Quality) | Milestone is additive control-plane focus | Wire new actions into existing pages where they belong (kill → RunDetail) |

---

## 8. Feature Dependencies

```
Cockpit Trigger ──→ needs budget pre-flight read ──→ BUDG-01 caps query API
      │
      └──→ runs.trigger_source='manual' (migration)
Webhook Inbox ──→ needs ListTriage HTTP surface (new) + redelivery endpoint (new)
Apps Registry ──→ needs admin permission routing (admin.e2ai.admin in use)
Jobs Page ──→ needs read-only river_job query surface (new)
All mutations ──→ audit identity (existing httpx identity)
```

## 9. MVP Recommendation for the Milestone

Prioritize (in roadmap order, dependency-driven):
1. **Cockpit trigger + simulate** (§2, §6.1) — the headline feature; exercises budget pre-flight + manual source migration.
2. **Webhook inbox + redeliver + triage surface** (§4) — highest operational pain today (debug = psql); endpoint gap is small.
3. **Outside actions consolidation** (§6 rows 2–5, 7, 11) — mostly wiring existing endpoints into UI.
4. **Apps registry + jobs page** (§5, §3) — admin-permission CRUD and job ops; largest new API surface.

Defer: per-app config editing beyond thresholds/toggles (workflow/env changes are rare), bulk redeliver, job cancellation (retry + observe first), audit log viewer page (audit writes land in v1.1; a dedicated viewer can follow).

## 10. Sources

- Code (read directly, 2026-09-22): `web/src/app/routes.tsx`, `web/src/lib/api.ts`, `web/src/features/**` (Runs/Plans/Approvals/Corpus/Quality pages), `api/cmd/e2ai-api/main.go` (periodic wiring), `api/internal/ingest/{http,sweep,triage,dedupe}.go`, `api/internal/agents/{http,sweep}.go` (kill/enqueue/orphan), `api/internal/plans/http.go` (TriggerSync, gate), `api/internal/suite/canary.go`, `api/internal/app/migrations/000{1,2}.sql`, `api/internal/platform/httpx/auth.go` (permission ladder), `Taskfile.yml`.
- Sketches (read directly): `.planning/sketches/MANIFEST.md` + winners 001 (Live Feed), 002 (Screenshot Canvas), 004 (Corpus side-by-side), 006 (plan format); 005 (Registry Cards) informs §5.
- Planning docs: `.planning/PROJECT.md` (v1.1 milestone definition), `.planning/REQUIREMENTS.md` (45 v1 reqs, IDs referenced: INGE-01..03, EDGE-01, APPS-01..03, AGNT-02/04, BUDG-01..02, SAFE-01, PLAT-01..05, TREN-01, MCP-01).
- Confidence: HIGH for current-state claims (all grounded in code reads); MEDIUM for UX proposals (design judgments, consistent with sketch winners and shared-admin conventions).
