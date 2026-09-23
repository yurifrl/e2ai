# Project Research Summary

**Project:** E2AI (`@admin/e2ai`) — milestone v1.1 Admin Cockpit
**Domain:** Control-plane cockpit for an autonomous AI QA arm — admin UI to trigger, observe, and operate an existing report-only QA pipeline
**Researched:** 2026-09-22
**Overall confidence:** HIGH — all four research files are repo-grounded with file:line citations; River v0.47.0 APIs verified against vendored module source; zero web-sourced claims.

## Executive Summary

v1.1 is a **zero-new-dependency milestone**. The cockpit is a new web feature section plus a new Go surface package, both built strictly from patterns that already exist in this repo; the only genuinely new pipeline artifacts are one schema migration and a few new registry fields. The defining synthesis finding cuts across STACK and ARCHITECTURE independently: **this module deviates from the shared-admin family's named data stack, and v1.1 must follow module reality.** PROJECT.md's constraint says "React + RTK feature slices (web), Connect-RPC (api)" — but `web/package.json` has no redux/rtk and `api/go.mod` has no connectrpc. Reality is React 18 + `@nsxbet/admin-sdk` + hand-rolled fetch (`web/src/lib/api.ts`) + visibility-aware `usePolling` on the web side, and Go `net/http.ServeMux` REST with DTOs locked verbatim to `web/src/lib/types.ts` on the API side. Both researchers converged on the same recommendation: **log a PROJECT.md decision amendment** — "e2ai follows the admin family's *integration* conventions (manifest registration, BFF routing, identity headers) but uses net/http + fetch/polling for data, per its locked v1.0 contract" — so the roadmapper does not invent RTK/Connect phases. Introducing either mid-milestone would put a second data paradigm in one module or force rewriting every existing surface.

The new surface is small and enumerable. Schema: migration `0010_cockpit.sql` with exactly three changes — widen `runs_trigger_source_check` with `'manual'`, add `registered_apps.github_hook_id bigint`, add a `manual_run_seq` sequence (start 10^12) for synthetic workflow-run ids. Code: a `TriggerManual` constant, a trigger endpoint that mirrors the existing transactional Ingest seam (run row + `ExecuteArgs` enqueue in one tx), and thin REST handlers over capabilities that already exist. River v0.47.0 already provides the entire jobs observatory for free — `JobList`, `JobRetry`, `JobCancel`, `JobGet`, `QueueList`, `QueuePause`, `QueueResume` verified in vendored source; no Pro features needed. The webhook inbox's biggest blocker dissolves on inspection: `webhook_events`/`webhook_triage` already store everything; `ListTriage` exists but is **unmounted** (no HTTP handler in `main.go`) — the gap is one mount plus a retry endpoint.

The dominant risk is that the cockpit makes *spending money* cheap. The budget engine treats ceiling `0` as unlimited, today's only enqueue path never writes a cost ceiling, and its dedupe is a racy read-then-insert — so a naively-wired trigger button can mint unbounded, duplicated paid sessions (PITFALLS P1–P3). Every trigger-shaped phase must validate both ceilings strictly positive, enforce an aggregate app/run budget gate at trigger time, and make the insert itself the concurrency gate. Secondary risks with documented preventions: approval-version races (P4 — pin version + content hash, never re-resolve "current approved" at spawn), sticky-PR-comment clobbering (P6 — manual runs must never PATCH a deploy run's sticky comment), and 202-then-silence UX (M1 — create the run row synchronously in the same tx as the enqueue).

## Key Findings

### Recommended Stack (from STACK.md)

**Zero new dependencies** — every candidate explicitly rejected: RTK/RTK Query (no Redux exists; two paradigms in one module), Connect-RPC (zero usage; wire contract is locked `types.ts` ↔ Go DTOs), `go-github` (hand-rolled client with rate-limit accounting; one more endpoint is ~30 lines), SSE/TanStack Query (`usePolling` was built for exactly this), modal libraries (two-step arm-confirm buttons are the house pattern).

**Core technologies (all already pinned in the repo):**
- **React 18 + react-router-dom 6.28 + `@nsxbet/admin-sdk` 0.14** — cockpit routes in `web/src/app/routes.tsx`, manifest command entry in `web/admin.module.json`, `useFetch`/`useI18n`/`env` from the sdk; 4-locale i18n (en-US, pt-BR, es, ro) on day one.
- **Existing fetch client + `usePolling`** — all cockpit data via `lib/api.ts` `getJson`/`postJson` extended with cockpit endpoints; 10s list / 5s detail cadence, visibility-aware.
- **Mutation UX precedent** — `ApprovalsQueuePage.tsx` `busyId`/`actionError` pattern + two-step confirm for destructive actions (kill, pause, redelivery); errors surfaced verbatim (404/409/403/503).
- **`net/http` ServeMux REST** — new handlers mirror `flakes.QuarantineHTTP` shape: `Mount(mux)`, method-pattern routes, `gate(w,r,perm)`, `writeErr`; mounted in `main.go` `newMux` behind the nil-guard like siblings.
- **River v0.47.0 client methods** — jobs observatory over the api's existing client: `JobList` (States/Kinds filters + cursor pagination), `JobRetry`, `JobCancel`, `QueuePause`/`QueueResume`/`QueueList`. Cockpit controls *job instances and queues*; periodic-job *schedules* stay in code.
- **Ingest re-drive for webhook retry (primary)** — re-parse stored `webhook_events.payload`, re-drive the same `Ingest` seam with `retry` provenance; transactional and idempotent by construction. GitHub-native redelivery via extended `platform/github` client is the secondary action (needs `github_hook_id`).

### Expected Features (from FEATURES.md)

The outside-action audit enumerated **16 operator actions**; rows 13–16 (alert ack, quarantine, corpus promote, approvals) are already in the UI — v1.1 must not regress them. **The deliverable is rows 1–12.**

**Must have (the v1.1 deliverable):**
- Command Center home (sketch 001 winner A · Live Feed): KPI header (runs today, pass rate, pending approvals badge, **budget spend vs cap**, **paused/kill state**), reverse-chron feed, agent rail with stuck-session flags, app filter chips — with the trigger affordance that turns the feed into a cockpit.
- Manual trigger (§2): picker defaults to latest **approved** plan version; draft/superseded versions render disabled *with a one-line reason* (approval invariant made visible, not hidden); confirm screen shows pinned version identity + current budget state; run consumes normal budget (no free path); bypasses wait-rollout (app is already deployed).
- Webhook simulate: api renders + HMAC-signs the payload server-side (secrets never reach the browser); result shows `ingested / already-ingested / filtered` + run link.
- Webhook inbox (§4): deliveries + first-class UNCLAIMED triage rows, raw-JSON payload viewer with copy (today devs curl the DB), redeliver with 5s cooldown, "Register app" funnel pre-filled from delivery payload.
- Outside actions (§3/§6): agent enqueue + kill on RunDetail, plans-sync buttons, MCP probe (read-only, no budget), canary run-now.
- Apps registry (§5): registry cards, add/edit wizard — **secrets are vault refs only** (`webhook_secret_ref op://` unchanged per SECR-01), server-side resolution check, two-step delete.
- Jobs observatory: River job list, retry/cancel per-kind semantics, queue pause/resume with persistent-pause banner, canary card (inert vs active).

**Defer (post-v1.1, per FEATURES §9):** per-app config editing beyond thresholds/toggles; bulk redeliver; **job cancellation** (retry + observe first — capability is free, semantics are heavy); audit log viewer page (audit *writes* land in v1.1, viewer follows).

**Anti-features (binding):** no cockpit mutation of the app under test; no auto-approval; no unlimited triggers; no raw secret entry/display; no free-form SQL/job console; no rework of v1.0 pages (Runs/Plans/Corpus/Quality stay additive-only — new actions wire into existing pages where they belong, e.g. kill → RunDetail).

### Architecture Approach (from ARCHITECTURE.md)

Cockpit actions land in owning domains (hexagonal match is exact); the web side adds a `features/cockpit/` folder mirroring the one-folder-per-surface shape. The trigger path reuses two verified precedents composed: the canary's out-of-webhook run creation (`suite/canary.go`) × the derive-draft endpoint's admin-gated enqueue shape (`main.go:405-421`).

**Major components:**
1. **Manual trigger endpoint** — one tx mirroring `ingest/dedupe.go`: run row (`trigger_source='manual'`, synthetic id, operator attribution) + `ExecuteArgs` enqueue atomically (runner queue, `MaxAttempts: 1`, `UniqueOpts.ByState`). Synchronous run-row creation is what kills M1's 202-then-silence. If the app has no approved plan version anywhere → 409 (same condition `resolvePlanGate` treats as `draft_only`-fail); approval gating is never bypassed.
2. **Cockpit REST surface** — plain net/http behind BFF identity (`httpx.IdentityFromContext`); every write records the actor; single shared `gate` helper instead of hand-rolled per-handler switches.
3. **Jobs observatory** — River client reads + control over the api's existing client; per-kind semantics table is the core design artifact (see retry rule below).
4. **Webhook inbox** — read surface over `webhook_events`/`webhook_triage`; `ListTriage` exists unmounted; retry = ingest re-drive (primary) + GitHub redelivery (secondary, needs `github_hook_id`).
5. **Registry surface** — cards + wizard over `registered_apps` (already seeded from `registry/*.yaml` at boot via `app.Bootstrap`); source-of-truth tension flagged below.

**The cockpit retry rule (ARCH §3.5, load-bearing for the jobs page):** retry of a *run* = fresh manual run (terminal run states have no outgoing edges; `JobRetry` on an execute job re-enters the executor against a terminal run → typed rejection → job re-errors). Job-level retry is legitimate **only for default-queue kinds** (`e2ai:run:report`, `e2ai:plans:sync`, `e2ai:derive:draft`). Cancel of a running execute is cooperative (River NOTIFY → runner ctx cancel → docker run killed) and must be paired with a guarded run-row transition — cancelling the job alone strands the run row non-terminal forever.

**Minimal schema surface (migration `0010_cockpit.sql` — everything v1.1 must add):**
1. Widen `runs_trigger_source_check` with `'manual'` (0007 precedent: constraint widened in the same migration as the feature; Go constant `TriggerManual` alongside).
2. `registered_apps.github_hook_id bigint` (+ optional `hook_created_at`); field added to `registry/*.yaml` (`betweb.yaml.example`, `bet-app.yaml`); secret stays `op://` per SECR-01.
3. `manual_run_seq` sequence starting at 10^12 for synthetic workflow-run ids — GitHub ids are ~10^9 today; the reserved band keeps `unique (app_id, workflow_run_id)` untouched (dedupe depends on it). All three research files agree on the *invariant* (disjoint from GitHub's positive id space, deterministic generator, widened in the same migration); STACK's dedicated sequence is the primary recommendation (collision-proof, decades of headroom), ARCH's reuse of the canary's negative-id precedent is the acceptable fallback if the planner prefers no new sequence object. Pitfall P5 requires a fail-closed regression test: a manual-source insert *before* the migration must violate the constraint.

**No new permission strings.** The ladder is `admin.e2ai.view` / `admin.e2ai.edit` / **`admin.e2ai.admin`** — the third rung is *already defined at `httpx/auth.go:37` and has never been used; v1.1 is its first consumer* (registry CRUD, job control, queue pause, redelivery, triage promotion). Triggers are edit-tier. Two wiring notes: the dev vite proxy must extend its header list (`web/vite.config.ts`, one line) so local dev can exercise edit/admin-gated actions, and staging must verify the full BFF → `X-Permissions` header → api gate chain for the admin rung (Okta scope config lives in `config/admin-bff*.yaml`) as a phase acceptance criterion, not an assumption.

### Critical Pitfalls (from PITFALLS.md — the set that shapes phase 1)

1. **P1 — ceiling `0` means unlimited.** The budget engine skips enforcement when `step_ceiling = 0` or `cost_ceiling_millicents = 0`; today's only enqueue path never writes a cost ceiling and a zero-step approved plan would mint unlimited-step sessions. The trigger path must (a) reject zero-step plans before enqueue, (b) always write an explicit positive cost ceiling from app config or a manual-trigger cap, (c) treat ceiling-0 as a validation error at the *trigger API boundary* (engine sentinel semantics stay; no caller may pass 0).
2. **P2 — budget enforcement is per-session only.** Two sessions per enqueue, nothing sums them; ten parallel runs = twenty independent budgets, each "within ceiling." Add an **aggregate app/run-level budget gate at trigger time** (409 + surfaced reason when breached), reading the same `agent_sessions` aggregates the gate enforces so the UI pre-flight and the engine agree on one source of truth.
3. **P3 — enqueue dedupe is read-then-insert.** Concurrent triggers race into duplicate paid sessions (or a raw 500). Make the insert the gate: conflict-do-nothing on `(run_id, role)` mapped to 409, or River `UniqueOpts` as `derive/job.go` already does — **from day one on the trigger path**. Test: two parallel enqueues → exactly one 202, one 409.
4. **P4 — approving a plan mid-flight flips the gate.** Pin version id + content hash at trigger time; confirm screen shows the hash that will run; spawn gate accepts the pinned hash even if that version was superseded after run creation — never re-resolve "current approved" at spawn for a pinned run.
5. **P6 — sticky PR comment is last-writer-wins.** A manual run on the same SHA races the deploy run to PATCH the same sticky comment. Gate sticky-comment publishing by `trigger_source` (deploy runs only) and give manual runs a run-detail-only report surface. Test: webhook run A + manual run B on one SHA → B never PATCHes the comment; `pr_comments.last_run_id` still points at A.

Phase-shaping secondary set: **M1** (synchronous run row + job-liveness surfacing; derive-draft should return the run id), **M2** (cancel is one operation over the run+sessions aggregate — kill each non-terminal session tolerating 409-as-success, then guarded run transition; idempotent re-cancel), **M3** (consider a dedicated River queue for cockpit-triggered jobs so operator latency is independent of pipeline load), **M4** (audit row — actor, action, target — written in the same tx as every cockpit mutation; ingest tx is the template), **M5/M6** (no-header mutation requests must be rejected in non-dev mode; `DevFallbackUser` must not default-grant the new rungs), **M7/m1** (all four locale files share the same key set — fold into existing manifest validation), **m3** (post-mutation immediate refetch; hidden-tab polling pause makes an operator believe a trigger failed).

## Implications for Roadmap

Suggested 4 phases — the MVP ordering consensus from FEATURES §9, made dependency-explicit:

### Phase 1: Cockpit Trigger + Simulate + Command Center
**Rationale:** the headline feature, and the phase that mints the shared substrate — migration 0010, budget-gate pattern, permission wiring, audit pattern — that every later phase reuses. The Command Center (001-A) is the integration point every other feature reports into; FEATURES is explicit that it must exist first or the other features have no home.
**Delivers:** `0010_cockpit.sql`; `TriggerManual`; `POST /api/cockpit/triggers` + webhook-simulate endpoint (both driving the Ingest seam); trigger-time budget gate (P1–P3 preventions); Command Center page with trigger dialog, KPI header, live feed.
**Addresses:** FEATURES §1, §2, §6.1 (audit rows 1, 6). **Avoids:** P1, P2, P3, P4, P5, P6, M1, M4 (audit from day one).

### Phase 2: Webhook Inbox
**Rationale:** highest operational pain today (inbox debugging = psql/curl), and the smallest endpoint gap in the milestone — the tables and the `ListTriage` query already exist; only the HTTP mount, a read surface, and a retry endpoint are missing.
**Delivers:** `GET /api/cockpit/webhook-events` + `/api/cockpit/triage` (mounts `ListTriage`); inbox UI (deliveries + UNCLAIMED triage rows, raw payload viewer, filters, polling); ingest re-drive retry; GitHub redelivery action (uses `github_hook_id`).
**Addresses:** FEATURES §4 (rows 5, 8). **Avoids:** P6 copy rules; m2 (route via BFF `e2aiApiBase`, never the module origin's `/api/`).

### Phase 3: Outside-Action Consolidation
**Rationale:** mostly wiring existing endpoints into UI; reuses phase-1's gate helper, audit pattern, confirm pattern, and post-mutation refetch with no new pipeline surface.
**Delivers:** RunDetail "Add agent session" (role picker) + kill button; plans-sync buttons (Plans page + app card — `syncAppPlans` is exported but currently unused by any page); MCP probe with response viewer; canary run-now.
**Addresses:** FEATURES §3, §6 rows 2–4, 7, 11. **Avoids:** M2 (guarded cancel semantics), m3 (immediate refetch).

### Phase 4: Apps Registry + Jobs Observatory
**Rationale:** the largest new API surface; the `admin.e2ai.admin` rung gets its heaviest consumers here (registry CRUD, job control, queue pause); needs the River control-semantics questions settled first.
**Delivers:** registry cards + add/edit wizard (vault-ref secrets, server-side resolution check); River jobs page (`JobList` + retry/cancel + `QueuePause`/`QueueResume` with persistent-pause banner); canary card.
**Addresses:** FEATURES §5, §3 jobs (rows 9, 10, 12). **Avoids:** queue-pause blast-radius mislabeling (pausing `default` silently stalls wait-rollout — banner + `admin` gating), M5 permission-matrix tests over the full route map.

### Phase Ordering Rationale
- Trigger first because the schema migration + budget gate + audit row are the substrate the inbox retry, consolidation actions, and registry all inherit; inbox second because pain-per-effort is maximal there; consolidation third because it is pure wiring; registry/jobs last because they are the largest and carry the open design decisions (below).
- v1.0 surfaces are additive-only throughout: new actions attach to existing pages (kill → RunDetail, sync → Plans) instead of reworking them.
- All cockpit mutations are `admin.e2ai.edit`-or-admin gated with BFF-identity attribution; the first phase to consume the admin rung (whichever lands first) owns the staging verification of the BFF → header → gate chain.

### Research Flags

Phases likely needing deeper research during planning (`/gsd:plan-phase --research-phase`):
- **Phase 1:** budget-gate shape (cap source: app config column vs new table), P6 reporting-contract decision (which of a/b/c), `ExecuteArgs` extension for pinned `plan_version_id` (ARCH explicitly flags this as a planner decision), trigger-job queue choice (dedicated vs default — M3), final `workflow_run_id` scheme call (sequence vs canary-negative).
- **Phase 4:** River running-job cancel propagation end-to-end (runner client listener config — MEDIUM confidence per ARCH; HIGH per-link).

Phases with standard patterns (skip research-phase):
- **Phase 2:** read surfaces + polling + row-expand follow the established report-page patterns; dedupe semantics are already in the code path being re-driven.
- **Phase 3:** pure endpoint→UI wiring of existing handlers (`agents/http.go`, `plans/http.go` sync, `mcp/http.go`, canary trigger).

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | Every claim repo-verified with file paths; River v0.47.0 verified against vendored source; zero web-sourced claims |
| Features | HIGH (current-state) / MEDIUM (UX) | Outside-action audit is code-grounded; UX behaviors are design judgments consistent with sketch winners 001/002/004/005/006 |
| Architecture | HIGH | file:line-verified integration map; two MEDIUM items: end-to-end cancel propagation, replay identity minting |
| Pitfalls | HIGH | P1–P6 quoted from shipped code; MEDIUM on permission/BFF blast radius; LOW where marked [INFERENCE] (worker counts M3, quarantine actor column M4) |

**Overall confidence:** HIGH

### Gaps to Address

- **Registry source of truth:** `registered_apps` is seeded from config-as-code `registry/*.yaml` at boot; UI CRUD (phase 4) needs an explicit decision — YAML stays canonical (UI read-only or generation tooling) vs DB becomes canonical (boot seeding demoted). This same decision bounds triage-promotion design (ARCH §9 flags it).
- **River cancel propagation:** verify the runner client's listener control-event config in phase 4 planning — or defer `JobCancel` entirely (FEATURES already defers it; retry + observe first).
- **`ExecuteArgs` version pinning:** manual triggers today resolve through the executor's unchanged `resolvePlanGate`; pinning one `plan_version_id` requires extending `ExecuteArgs` — phase 1 planning decision.
- **Aggregate budget cap source:** where the app-level cap lives (app config column vs new table) — phase 1 planning; the gate and the UI pre-flight must read the same source.
- **`DevFallbackUser` grants:** verify the dev fallback identity does not default-grant `admin.e2ai.edit`/`admin.e2ai.admin` (M5/M6); non-dev mode must reject header-less mutation requests.
- **Staging permission-chain check:** BFF Okta scope config for the `admin.e2ai.admin` rung (`config/admin-bff*.yaml`) — phase acceptance criterion for the first phase consuming it.
- **Quarantine actor column:** M4's INFERENCE that `test_quarantine` may already carry `activated_by` — verify against `0007_hardening.sql` during the consolidation phase audit work.

## Sources

All four research files are repo-grounded with inline file:line citations (2026-09-22); no external web sources were needed.

### Primary (HIGH confidence)
- Repository code: `web/{package.json,admin.module.json,vite.config.ts}`, `web/src/{app,lib,shared,features/**,i18n}`, `api/go.mod`, `api/cmd/e2ai-api/main.go`, `api/internal/{ingest,plans,approvals,agents,flakes,trends,mcp,report,runs,suite,derive,platform/{httpx,github,river},app/migrations}`, `registry/*.yaml`, `config/admin-bff*.yaml`, `Tiltfile`
- River v0.47.0 vendored source (module cache): `client.go`, `job_list_params.go`
- Planning docs: `.planning/PROJECT.md` (v1.1 charter), `.planning/REQUIREMENTS.md` (v1: 45 reqs closed — all cockpit features build on shipped v1.0 surfaces), git-history v1.0 `PITFALLS.md` (2026-09-19; amplification table in v1.1 PITFALLS maps cockpit × v1.0 agent-domain risks)
- Sketches: `.planning/sketches/MANIFEST.md` + winners 001 (Live Feed), 002, 004, 005 (Registry Cards), 006

---
*Research completed: 2026-09-22*
*Ready for roadmap: yes*
