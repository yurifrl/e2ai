---
phase: 02-plans-approvals
plan: 02-01-PLAN.md
status: passed
verified: 2026-09-20
method: automated-tests-and-live-smoke
---

# Phase 2 Verification Report

## Goal-backward check

**Goal:** human-supervised autonomy — declarative plans with content-hash identity,
approval queue gating the pipeline, PR-diff derivation (deterministic drafts, never
auto-run), corpus oracle seeded from Playwright specs.

Every plan wave produced tested code; the full `go test ./... -count=1` suite is green
across all packages (app, approvals, corpus, derive, ingest, platform/httpx, report, runs,
suite, verdict). The phase gate (T9) additionally proved the chain end-to-end in the
`api/internal/ingest` e2e test: webhook → plans sync → plan pending → approve via REST with
identity → second webhook executes approved version only → changed plan re-queues pending
while the previously approved version keeps running.

## Verification by requirement

- **PLAN-01/02** (plan format + registry sourcing): parser unit tests; registry
  `plans_source` seed round-trip in `internal/app` tests; migration up/down + boot-twice
  idempotency on scratch postgres.
- **APPR-01/02** (approval queue + attributed decisions): approval state-machine tests;
  HTTP handler tests (identity attribution incl. dev-fallback); REST endpoints verified
  **live**: `GET /api/approvals` returned the populated queue (200) with dev-fallback
  identity against scratch postgres.
- **DERV-01..03** (PR-diff derivation): detection/subset unit tests; derive-draft River job
  covered in `internal/derive` tests; manual re-derive REST endpoint mounted.
- **CORP-01..02** (corpus oracle): behaviors/learnings idempotency + promote-provenance +
  append-only tests; `GET /api/corpus?app=NSXBet/bet-app` returned 200 JSON live.

## Gate fixes applied at verification

The approvals HTTP surface existed and was unit-tested but was never mounted in the live
binary (`api/cmd/e2ai-api/main.go`). Wired into `newMux` with the plans surface's degraded
posture; verified live via the smoke described above, then full suite re-run green.

## Live smoke evidence

- API booted against scratch postgres (localhost:55432), seeded registry (2 apps).
- `GET /api/approvals` → 200, queue populated from plans-repo source.
- `GET /api/plans` → 200, plan catalog with content-hash identity fields.
- `GET /api/corpus?app=NSXBet/bet-app` → 200.
- Web build: `tsc && vite build` clean; `validate-manifest.ts` OK.

## Gaps

None outstanding for the phase's locked scope.
