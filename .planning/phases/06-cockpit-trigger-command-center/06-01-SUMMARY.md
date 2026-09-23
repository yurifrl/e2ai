---
phase: 06-cockpit-trigger-command-center
plan: 01
subsystem: cockpit
tags: [cockpit, audit, budget-gate, pause-control, migration, wave-1]
requires:
  - 0001_init (runs / registered_apps / agent_sessions schema)
  - 0007 runs.trigger_source check constraint (drop-and-re-add pattern)
provides:
  - migration 0010: audit_events, cockpit_state, runs.trigger_source +manual/simulate, runs.triggered_by, cockpit_run_ids sequence, one-manual-in-flight partial gate index, registered_apps.budget_cap_millicents
  - api/internal/cockpit: AppendAudit / Read / SetPaused / CapFor / DaySpend / Evaluate
  - HTTP: GET /api/cockpit/state, POST /api/cockpit/pause, POST /api/cockpit/unpause (200 + fresh state)
  - web: CockpitState wire DTO, getCockpitState/pauseCockpit/unpauseCockpit, KpiCards header-zone shell + pause control, 4-locale strings
affects: [06-02 (manual trigger consumes gate index + Evaluate), 06-03 (simulate consumes audit + state), 06-04 (Command Center extends KpiCards)]
tech-stack:
  added: []
  patterns:
    - single-row upsert state (boolean PK guard)
    - same-transaction domain write + audit append (AUDT-01)
    - layered cap resolution (app column → env floor → typed no-cap)
    - day-scoped (UTC) spend aggregate from agent_sessions
    - two-step pause confirm with server-ack wait (no optimistic UI)
key-files:
  created:
    - api/internal/app/migrations/0010_cockpit.sql
    - api/internal/cockpit/audit.go
    - api/internal/cockpit/state.go
    - api/internal/cockpit/budget.go
    - api/internal/cockpit/service.go
    - api/internal/cockpit/http.go
    - api/internal/cockpit/state_test.go
    - api/internal/cockpit/budget_test.go
    - web/src/features/cockpit/KpiCards.tsx
  modified:
    - api/internal/app/postgres.go
    - api/cmd/e2ai-api/main.go
    - web/src/lib/types.ts
    - web/src/lib/api.ts
    - web/src/i18n/locales/{en-US,pt-BR,es,ro}.json
decisions:
  - "Pause/unpause mutations answer 200 + fresh CockpitState (house mutation convention; bare 204 broke postJson)"
  - "pauseCockpit wire body is reason-only; the route picks the direction (mirrors locked http.go, not the plan's {paused, reason} sketch)"
  - "Zero ceiling is a typed refusal candidate (BUDG-03); the engine's 0=unlimited session sentinel stays untouched"
  - "Test scratch-DB wipe uses TRUNCATE ... CASCADE (DELETE ... CASCADE is not valid SQL)"
metrics:
  duration: ~55m
  completed: 2026-09-23
status: complete
actuals:
  tokens: 12400
  tasks: 3
  commits: 3
  plan_head_before: 806df9cf1f92c7fcc0db7be484abd133cef81e47
---

# Phase 6 Plan 01: Cockpit Control-Plane Substrate Summary

**One-liner:** Phase 6 substrate minted once — migration 0010 (audit_events, single-row arm state, manual/simulate run vocabulary, trigger attribution, gate index, per-app budget cap), the budget gate domain (layered caps + UTC day spend), and the cockpit HTTP + web pause control with same-transaction audit attribution.

## What Was Built

- **Migration 0010 (all seven Phase 6 objects):** `audit_events` (append-only, closed action vocabulary), `cockpit_state` (single-row boolean-PK arm state), `runs.trigger_source` widened to `webhook|reconcile|sweep|manual|simulate`, `runs.triggered_by` operator column, `cockpit_run_ids` negative sequence, partial unique index `runs_one_manual_in_flight_per_app` (in-flight = status not in `completed|failed`), `registered_apps.budget_cap_millicents`.
- **Budget gate domain:** `CapFor` resolves app column → `E2AI_DEFAULT_BUDGET_CAP_MILLICENTS` env → typed no-cap (env garbage refuses loudly); `DaySpend` aggregates `agent_sessions.cost_millicents` over the UTC day; `Evaluate` returns cap/kind/spend/remaining with `Zero`/`Breached` verdicts ready for Plan 02's trigger boundary.
- **State store:** `Read` defaults unpaused on a missing row; `SetPaused` upserts the single row, refuses empty actors (`ErrNoActor`), and appends the `pause`/`unpause` audit row in the same transaction.
- **HTTP + web:** `Service.Mount` registers the three routes (view permission for reads, edit for mutations, placeholder identities refused); `KpiCards.tsx` renders the LIVE/PAUSED badge, the paused banner derived from the same poll, and a two-step pause (required reason, consequence sentence, button spins until the ack); strings in en-US, pt-BR, es, ro.

## Verification Results

- `go build ./...` + `go vet ./internal/cockpit/... ./cmd/e2ai-api/` — clean.
- Scratch postgres (PG14, ephemeral): migration 0010 applied from zero **three times** (fresh DBs) — all versions 1→10 apply.
- `go test ./internal/cockpit/` — **8/8 PASS** on real postgres (zero-ceiling refuses, uncapped never refuses, day-scope/breach boundaries, env-garbage cap, unpaused default, actor required, pause/unpause round trip with audit rows, upsert self-heal).
- Web gate: `bun run build` green + `bun run validate:manifest` — `manifest OK: @admin/e2ai (routeBase /e2ai)`.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Plan conformance] Migration 0010 initially minted only 3 of 7 Phase 6 objects**
- **Found during:** Task 2 read-first
- **Issue:** The migration was missing `apps.budget_cap_millicents`, `runs.triggered_by`, `cockpit_run_ids`, and the manual-gate partial index that Plans 02/03 consume (and must never re-create).
- **Fix:** Completed the migration with the four objects; semantics taken from Plan 02's truths (terminal states `completed|failed` per runs/state.go). Amended into T1's commit.
- **Commit:** 7eb9cf5b

**2. [Rule 1 - Contract] pause/unpause returned bare 204, breaking the web client**
- **Found during:** Task 3
- **Issue:** `postJson` unconditionally parses a JSON body; house convention (quarantine surface) is 200 + body.
- **Fix:** Mutations now answer 200 with the fresh `CockpitState` (one round trip, server-ack wait preserved).
- **Commit:** f22ff054

**3. [Rule 3 - Blocking] Test scratch-DB wipe used invalid `DELETE ... cascade`**
- **Found during:** Task 2 test run (FK violation: runs referencing registered_apps)
- **Fix:** `truncate registered_apps cascade` — one statement clears the app-dependent group.
- **Commit:** 2bb90566

### Documented Deviations

**4. [Deviation - plan premise vs locked code] `pauseCockpit({paused, reason})` → `pauseCockpit(reason?)`**
- **Issue:** The plan sketched `{paused, reason}` on the pause body, but the plan's own locked `http.go` parses a reason-only body (the route picks the direction). The wire contract wins ("DTO locked verbatim").
- **Resolution:** TS mirrors the wire; recorded in the broken-windows ledger.

## Auth Gates

None.

## Known Stubs

| Stub | File | Reason |
| ---- | ---- | ------ |
| KPI zone grid is an intentional shell (cards, not the pause control) | web/src/features/cockpit/KpiCards.tsx | Plan 06-01 delivers the header-zone shell + pause control only; KPI endpoints/cards mount in Plan 04. Documented in plan Task 3 action. |

## Threat Flags

None — no new trust-boundary surface beyond the plan's threat model (pause routes sit behind httpx edit permission; identities refused when placeholder).

## Self-Check: PASSED

- All created files exist (verified via commit contents + build).
- Commits 7eb9cf5b, 2bb90566, f22ff054 present on main (`git log` verified).
- Scratch postgres stopped; throwaway infra cleaned.
