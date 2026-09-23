---
phase: "6"
slug: "cockpit-trigger-command-center"
# status lifecycle: draft (seeded by plan-phase) → validated (set by validate-phase §6)
# audit-milestone §5.5 distinguishes NOT-VALIDATED (draft) from PARTIAL (validated + nyquist_compliant: false) (#2117)
status: draft
nyquist_compliant: false
wave_0_complete: false
created: "2026-09-23"
---

# Phase 6 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.
> Source: 06-RESEARCH.md §Validation Architecture (verified against ingest_test.go, e2e_test.go, runs/queries_test.go).

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Go `testing` (stdlib) + httptest; no web unit framework exists (build gate only) |
| **Config file** | none — convention-based (`*_test.go` beside code) |
| **Quick run command** | `go test ./api/internal/...` (DB-less; DB tests self-skip when `E2AI_TEST_DATABASE_URL` unset) |
| **Full suite command** | `go test ./...` + `cd web && bun run build && bun run validate:manifest` |
| **Estimated runtime** | ~30 s quick / ~2 min full |

**Harness facts** (planner must preserve):
- DB-backed tests require `E2AI_TEST_DATABASE_URL` and **skip silently when unset** (ingest_test.go:362-364) — never let a plan *require* DB e2e in environments without it.
- Fakes: `fakeStore`/`fakeEnqueuer` + `newTestHandler(store, queue)` build the ingest handler without River/DB (ingest_test.go:129-131) — trigger/simulate unit tests follow this.
- Pipeline e2e asserts by direct SQL (`select id from runs where workflow_run_id = 88001`, e2e_test.go:245-249) and trigger_source/delivery_id (ingest_test.go:540-544) — manual-trigger e2e assertions follow this shape.
- `seedApp`/`seedRun` helpers live in runs/queries_test.go for status-machine tests.

---

## Sampling Rate

- **After every task commit:** Run `go test ./api/internal/...` + the task's targeted new package test
- **After every plan wave:** Run `go test ./...` + `cd web && bun run build && bun run validate:manifest`
- **Before `/gsd:verify-work`:** Full suite green (+ DB-backed e2e with `E2AI_TEST_DATABASE_URL` when a test DB is reachable)
- **Max feedback latency:** 60 s quick / 120 s full

---

## Per-Task Verification Map

*Task IDs bind at validate-phase §6 after the planner assigns them; requirement rows below are the binding contract.*

| Task ID | Plan | Wave | Requirement | Threat Ref | Secure Behavior | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|------------|-----------------|-----------|-------------------|-------------|--------|
| TBD (planner) | TBD | W0 | BUDG-03 | — | ceiling-0 refused at trigger boundary; engine semantics untouched | unit | `go test ./api/internal/cockpit/ -run TestTriggerCeilingZero` | ❌ W0 | ⬜ pending |
| TBD (planner) | TBD | W0 | BUDG-03 | — | UTC-day aggregate vs cap → 409 with remaining reason | unit + DB e2e | `go test ./api/internal/cockpit/` ; `E2AI_TEST_DATABASE_URL=… go test ./api/internal/cockpit/ -run E2E` | ❌ W0 | ⬜ pending |
| TBD (planner) | TBD | W0 | TRIG-01..05 | — | run inserted at waiting-rollout, trigger_source='manual', synthetic ids, ExecuteArgs enqueued (runner queue) | unit + DB e2e | `go test ./api/internal/cockpit/ ./api/internal/ingest/` | ❌ W0 | ⬜ pending |
| TBD (planner) | TBD | W0 | TRIG (fail-closed) | — | `trigger_source='manual'` violates 0007 check pre-migration, passes after | DB e2e (constraint-level) | `E2AI_TEST_DATABASE_URL=… go test ./api/internal/app/migrations/ -run TestManualTriggerSource` | ❌ W0 | ⬜ pending |
| TBD (planner) | TBD | W0 | TRIG-05 | — | concurrent triggers: exactly one 202, one 409 (UniqueOpts shape, canary_test.go:92-95 pattern) | unit | `go test ./api/internal/cockpit/ -run TestConcurrentTrigger` | ❌ W0 | ⬜ pending |
| TBD (planner) | TBD | W0 | COCK-04 | — | pause blocks trigger + simulate; unpause audited; edit-rung enforcement 401/403 | unit | `go test ./api/internal/cockpit/ -run TestPause` | ❌ W0 | ⬜ pending |
| TBD (planner) | TBD | W0 | ACTN-01 | — | simulate HMAC-signs with resolved secret, drives real ingest, returns outcome + run link; blocked when paused | unit (sign() helper) | `go test ./api/internal/cockpit/ -run TestSimulate` | ❌ W0 | ⬜ pending |
| TBD (planner) | TBD | W0 | AUDT-01 | T-audit | audit row written in same tx as mutation; actor from identity; placeholder refused | unit | `go test ./api/internal/cockpit/ -run TestAudit` | ❌ W0 | ⬜ pending |
| TBD (planner) | TBD | W0 | COCK-02 | — | feed UNION endpoint returns runs/sessions/alerts, keyset-paginated | DB e2e | `E2AI_TEST_DATABASE_URL=… go test ./api/internal/cockpit/ -run TestFeed` | ❌ W0 | ⬜ pending |
| TBD (planner) | TBD | W0 | COCK-01/02 (web) | — | Command Center compiles; types.ts union extended not rewritten | build gate | `cd web && bun run build && bun run validate:manifest` | ✅ existing scripts | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

- [ ] `api/internal/cockpit/` package with surface + domain + tests (endpoint, gate, pause, simulate, audit, feed) — all Phase 6 Go tests land here
- [ ] DB e2e for trigger/simulate/pause following the ingest e2e harness (`E2AI_TEST_DATABASE_URL` bootstrap, e2e_test.go:245 pattern)
- [ ] Web: no test framework — rely on `tsc` build gate + manifest validation (do not introduce one this phase)

---

## Manual-Only Verifications

| Behavior | Requirement | Why Manual | Test Instructions |
|----------|-------------|------------|-------------------|
| Command Center visual layout matches locked sketch 006 (feed grid, agent rail, KPI cards, spacing/typography contract) | COCK-01 | No web test framework; visual contract is sketch-locked | Screenshot the running page from the Tilt dev stack; compare against `.planning/sketches/` winner 006 section-by-section (pagehead, chips, feed rows, rail cards) |
| Live BFF→API route through Tilt (`admin-bff.yaml` topology, host.docker.internal:8082) | COCK-01 | Requires running container stack; guardrail-gated to probe | Trigger one manual run through the UI against the Tilt stack; observe run appears in feed with trigger_source=manual |

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 60 s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
