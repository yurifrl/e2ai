---
phase: 06-cockpit-trigger-command-center
plan: 03
subsystem: api-cockpit-command-center
tags: [cockpit, feed, kpis, agent-rail, simulate, keyset-pagination, budget]
requires: [06-01, 06-02]
provides:
  - "POST /api/cockpit/simulate — server-side HMAC signed synthetic GitHub webhook, zero outbound calls"
  - "GET /api/cockpit/feed — three-source reverse-chron stream with keyset pagination + app filter"
  - "GET /api/cockpit/kpis — UTC-day-scoped header aggregates + arm-wide budget card"
  - "GET /api/cockpit/budget — per-app layered budget block (gate's own verdict)"
  - "GET /api/cockpit/agents — live session rail with >90s stuck predicate"
affects: [web-admin-command-center, cockpit-verifier]
tech-stack:
  added: []
  patterns:
    - "keyset pagination on (created_at, id) desc with opaque base64 cursor"
    - "per-UNION-arm predicate qualification ({APP}/{PAIR} template rendered per alias)"
    - "budget figures always via CapFor/DaySpend/Evaluate — the gate's own functions"
key-files:
  created:
    - api/internal/cockpit/simulate.go
    - api/internal/cockpit/simulate_test.go
    - api/internal/cockpit/feed.go
    - api/internal/cockpit/kpis.go
    - api/internal/cockpit/agents.go
    - api/internal/cockpit/feed_test.go
  modified:
    - api/internal/cockpit/http.go
    - api/internal/cockpit/service.go
decisions:
  - "Feed keyset cursor is opaque base64 of (RFC3339Nano stamp, uuid) — never OFFSET; predicates are built once with {APP}/{PAIR} placeholders and qualified per UNION arm because the sessions arm joins runs and both tables carry created_at/id"
  - "KPI pass rate = completed runs with verdict='pass' over completed runs; alert arrivals and sessions never inflate run counts"
  - "Arm-wide budget card reuses CapFor per registered app: capped caps sum; uncapped apps surface as a count, never a fabricated ceiling"
  - "Per-app budget block returns the gate's Evaluate verdict verbatim (cap kind, spend, remaining, refusal reason) — one source of truth with the 409 boundary"
  - "Agent rail drops terminal sessions (completed/failed/killed) and orders oldest heartbeats first; stuck = claimed|running with coalesced heartbeat older than 90s, mirroring agents/http.go"
  - "Simulate signs server-side through the real ingest seam (ingestHandler.SecretFor) — no second HMAC implementation, zero outbound GitHub calls"
metrics:
  duration: "~3h wall (resumed mid-plan after context compaction)"
  completed: 2026-09-23
status: complete
actuals:
  tokens: 15137
  tasks: 2
  commits: 4
plan_head_before: 1e85ef80638ab488f16bdd6eb36e089ee359f8cc
---

# Phase 06 Plan 03: Command Center read models — simulate, feed, KPIs, agent rail Summary

Simulate signs synthetic GitHub webhooks server-side and drives the real ingest seam with zero outbound calls; the feed stitches runs, agent sessions, and alert arrivals into one reverse-chronological keyset-paginated stream; the KPI header and per-app budget block reuse the trigger gate's own cap/spend functions; the agent rail lists live sessions with the admin queue's stuck predicate.

## Completed Tasks

| Task | Name | Commits | Key files |
| ---- | ---- | ------- | --------- |
| 1 | Simulate: synthetic payload factory + server-side HMAC + ingest seam drive | 9912576a (feat), 4441801d (test) | simulate.go, simulate_test.go |
| 2 | Feed UNION + KPI header + per-app budget + agent rail | 4766fa78 (feat), 977cc31c (test) | feed.go, kpis.go, agents.go, http.go, feed_test.go |

## Deviations from Plan

**1. [Rule 1 - Bug] Ambiguous `created_at` in the feed's cursor predicate (found by tests)**
- **Found during:** Task 2 — `TestFeedKeysetPagination` failed with SQLSTATE 42702.
- **Issue:** The same `where` fragment (built for the app filter and extended with the cursor pair) was spliced into all three UNION arms. In the sessions arm (`agent_sessions se join runs r`) both tables carry `created_at` and `id`, so unqualified references are ambiguous.
- **Fix:** Predicates are now built once from an `{APP}`/`{PAIR}` placeholder template and qualified per arm (`r.app_id` / `se.app_id`-via-join / `a.app_id`; `se.created_at, se.id` etc.).
- **Files modified:** api/internal/cockpit/feed.go
- **Commit:** 4766fa78

**2. [Rule 3 - Blocking] Missing route: `/api/cockpit/budget` was never mounted**
- **Found during:** Task 2 — Mount registered feed/kpis/agents but not the BudgetHTTP handler that already existed.
- **Fix:** Added `mux.HandleFunc("GET /api/cockpit/budget", s.BudgetHTTP)`.
- **Files modified:** api/internal/cockpit/http.go
- **Commit:** 4766fa78

Out-of-scope observation (not fixed, not staged): ~532 tracked files under `api/internal/suite/fixturegen/node_modules/` appear deleted in the working tree — someone wiped node_modules outside this plan. Deliberately left unstaged so the plan commits stay atomic.

## Authentication Gates

None — all work ran against the local scratch Postgres (`E2AI_TEST_DATABASE_URL=postgres://e2ai:e2ai@localhost:55432/e2ai_test`); the OrbStack-backed container port from the plan's premise (5434) was unavailable, so the scratch database was created on the live local postgres instance instead.

## Verification

- `go build ./... && go vet ./...` clean; `gofmt -l` empty for internal/cockpit.
- Full cockpit suite green against the scratch DB (`-p 1 -count=1`): 17 tests including cursor round-trip/malformed/limit-clamp units, keyset pagination across 3 pages with no overlap/repeat, app-filter narrowing, KPI day-scoping and pass-rate semantics, capped/uncapped budget verdicts, and rail ordering/stuck/drop-off behavior. Simulator tests (T1) green.
- DDL constraints anchored before seeding: runs.status/verdict check sets, agent_sessions.role/status sets, alert_events kinds (flake_threshold|flip_rate).

## Known Stubs

None — every surface reads real tables; no placeholder data paths.

## Self-Check: PASSED

- simulate.go, simulate_test.go, feed.go, kpis.go, agents.go, feed_test.go exist on disk.
- Commits 9912576a, 4441801d, 4766fa78, 977cc31c present in `git log`.
