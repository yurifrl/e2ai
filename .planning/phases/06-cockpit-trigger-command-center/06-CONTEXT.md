# Phase 6: Cockpit trigger, Command Center, and webhook simulate - Context

**Phase Goal:** Operators run and steer the arm from a single pane: manual triggering behind a visible pre-flight budget gate, a Command Center as the module's landing page, and server-side webhook simulation — the phase that mints the shared substrate (migration 0010, budget gate, audit attribution, permission wiring) every later phase reuses.

## Aligned Requirements
COCK-01..04, TRIG-01..05, ACTN-01, AUDT-01, BUDG-03

## Specifics (discuss 2026-09-23 — user delegated all four gray areas to recommendations; scout-verified against code)

**Gray-area decisions (user: "do it, finish it" — all recommendations locked):**

1. **Budget caps (BUDG-03):** per-app cap lives as a nullable `budget_cap_millicents` column on the app row in `0010_cockpit.sql` (editable later via P9 registry); a global default cap env var (`E2AI_DEFAULT_BUDGET_CAP_MILLICENTS`) floors uncapped apps. Uncapped = explicit "no cap" state in the confirm UI — never a refusal. Gate = `sum(agent_sessions.cost_millicents)` joined via runs for the app over the current UTC day vs cap; breach → 409 with remaining-budget reason; UI pre-flight shows spend/remaining before the click. Spend source verified: `report/page.go:175` (run totals) + `trends.RunCosts` — no stored per-day aggregate exists today (scout §4), so mint the query in the cockpit domain.
2. **Global pause (COCK-04):** single-row `cockpit_state` table in 0010 (paused flag, reason, operator, updated_at). Permission rung = `admin.e2ai.edit` (pause is steering, not registry governance — admin rung's first consumers stay in P9). Pause blocks **both** manual triggers and webhook simulate (paused = the arm accepts no new runs, full stop); pause/unpause is an audited mutation like every cockpit write; state surfaces in the KPI header + banner. In-flight runs are untouched (report-only).
3. **Manual run shape (TRIG-01..05):** mirror the deploy path 1:1 — trigger endpoint inserts the run row (`trigger_source='manual'`, synthetic id from a dedicated sequence — fail-closed uniqueness regression test proves pre-migration constraint rejection) + audit row in one tx, then enqueues `suite.ExecuteArgs` on the runner queue (`ingest/rollout.go:149` shape). No new execution semantics; no auto agent-session enqueue (P8 adds the attach action; today's sessions attach via existing `POST /api/runs/{id}/agent-sessions`). Plan pinning stamps `run_plan_resolutions` (version_id + content_hash, same shape `suite/execute.go:343` writes) and the spawn gate accepts the pinned hash even if superseded post-pin (P4) — never re-resolves "current approved" for pinned runs. Re-run appends: "same commit (reproduce)" pins original head SHA; "latest head (retest)" pulls current default branch.
4. **Command Center feed (COCK-02):** read-model UNION endpoint over existing tables (runs, agent_sessions, alert_events) — no `feed_events` write path, no write amplification; all event sources verified present (scout §10). Day-scoped KPIs (runs today, pass rate today) minted in the cockpit domain (`trends.HistoryRecent` is window-scoped, not day-scoped); pending-approvals badge reuses `GET /api/approvals`; budget KPI uses the same per-app/day aggregate as the gate.

**Substrate decisions (research-locked, restated for the planner):**

- `0010_cockpit.sql` is the next migration (0009 is highest; scout §1). Contents: widen `runs_trigger_source_check` (0007:59-63 pattern) with `'manual'`; audit attribution table (clean mint — **no audit table exists today**, scout §2; closest in-repo pattern `plan_approvals.approver/decided_at` written via `httpx.UserOf`); `cockpit_state` pause row; app budget-cap column; dedicated synthetic-id sequence.
- Trigger endpoint `POST /api/cockpit/triggers` `{app_id, head_sha, head_branch?, environment, reason?}` gated `httpx.RunEditPermission` (first consumer of `admin.e2ai.edit`; pattern: `agents/http.go` Enqueue + `approvals/http.go:267` attribution). Rejects: zero-step approved plans, ceiling-0 (validation error, engine sentinel untouched), paused arm, budget breach, zero-approved-plan apps (→ link to Approvals). Concurrent triggers serialize: one 202, one 409 (insert-as-gate or River `UniqueOpts`, P3 — `derive/job.go:40` pattern).
- Sessions minted by the trigger path always carry explicit positive ceilings (`step_ceiling = len(plan.Steps)`, cost ceiling from app cap) — P1.
- Webhook simulate (ACTN-01): server-side endpoint reusing the real ingest seam (`ingest.HandleEvent`); api renders payload + HMAC-signs with the app's resolved secret (secrets never reach the browser); result = ingest outcome + created-run link. Simulate ingest outcomes are visible in Command Center (sketch 001-A "simulate a bad deploy" row). Blocked when paused.
- Audit (AUDT-01): every cockpit mutation writes an audit row **in the same tx** as the mutation (ingest tx `dedupe.go` as template), via `httpx.IdentityFromContext(...).User`; UI answers "who/when" from the affected row.
- Comment discipline (P6 fix): sticky PR comment publishing gated to deploy-sourced runs (`trigger_source` check before `report/comment.go` Publisher PATCHes); manual runs report on Run Detail only; regression test: webhook run A + manual run B same SHA → B never PATCHes the sticky comment, `pr_comments.last_run_id` stays A.
- Permissions/dev loop: `web/vite.config.ts` proxy header list extended with `admin.e2ai.edit`; staging BFF scope config (`config/admin-bff*.yaml`) is a phase acceptance check. No new permission strings (`httpx/auth.go:35-37` already defines the rungs).
- UI conventions: `web/src/features/cockpit/` feature folder (page + `lib/api.ts` + `usePolling`), Command Center registered as module-root landing (`web/admin.module.json` nav/commands mirroring the 7 existing commands), polling cadence 10s list / 5s detail (`usePolling.ts` house pattern), `web/src/lib/types.ts` extended (never rewritten — locked contract), `RunDetailPage.tsx` gains `trigger_source` display (currently unrendered — scout §8).

**Canonical refs (planner must read):** `.planning/research/ARCHITECTURE.md` (§2 trigger path, §3 manual-trigger recipe), `.planning/research/STACK.md` (River methods, permission ladder, admin-sdk exports), `.planning/research/PITFALLS.md` P1-P6, `.planning/sketches/001-command-center` (winner: variant A), roadmap Phase 6 deliverables + success criteria, scout findings (integration seams: `dedupe.go:218-233`, `rollout.go:149`, `suite/execute.go:343-420`, `agents/http.go:337` stuck flag, `main.go:174-196` worker wiring, `main.go:338-339` webhook routes).

## Boundaries
- No admin-rung actions (P8: agent attach/cancel; P9: registry CRUD, queue pause/resume) — only `admin.e2ai.edit` this phase.
- No webhook inbox UI (P7), no app registry wizard (P9), no jobs observatory (P9).
- Mobile surfaces untouched (Phase 4 shipped them; cockpit is web + api).
- No auto-approval — Approvals queue stays the only approval path. Report-only invariant: trigger/simulate/pause actuate the arm, never the app under test.

## Deferred
- Auto agent-session enqueue on manual trigger (P8 "Add agent session" action covers attachment).
- Per-app cap editing UI (P9 registry wizard; cap column + env floor land now).