---
phase: 06-cockpit-trigger-command-center
plan: 02
subsystem: cockpit
tags: [cockpit, manual-trigger, budget-gate, plan-pin, sticky-comment, river]
requires:
  - 06-01 migration 0010 (cockpit_run_ids sequence, one-manual-in-flight partial index, budget_cap_millicents)
  - 06-01 internal/cockpit Evaluate / SetPaused / AppendAudit
provides:
  - cockpit.Trigger — atomic tx (validate → gates → insert-as-gate → plan pin stamp → audit → River enqueue)
  - HTTP POST /api/cockpit/triggers (200 + fresh state conventions, typed refusals)
  - GitHubHeadResolver (default-branch head SHA via repos/…/commits/<default_branch>)
  - spawn-gate pin acceptance: resolvePlanGate reuses the stamped approved resolution verbatim
  - publisher gates the PR-comment leg on trigger_source=webhook
affects: [06-03 (simulate feeds the same gates), 06-04 (TriggerPanel consumes state shape), 06-05 (TriggerPanel action)]
tech-stack:
  added: []
  patterns:
    - insert-as-gate (bare on-conflict-do-nothing inside the trigger tx = concurrency gate)
    - trigger-time plan pin stamped into run_plan_resolutions (mode approved, source manual_pinned)
    - negative synthetic workflow_run_ids from cockpit_run_ids sequence (never collides with webhook dedupe key)
    - comment leg gated on trigger_source (deploy comment identity preserved)
key-files:
  created:
    - api/internal/cockpit/trigger.go
    - api/internal/cockpit/trigger_gates.go
    - api/internal/cockpit/trigger_http.go
    - api/internal/cockpit/trigger_test.go
    - api/internal/app/migrations/migrations_test.go
  modified:
    - api/internal/cockpit/service.go
    - api/internal/cockpit/http.go
    - api/cmd/e2ai-api/main.go
    - api/internal/platform/github/client.go
    - api/internal/suite/execute.go
    - api/internal/suite/resolution_test.go
    - api/internal/report/comment.go
    - api/internal/report/comment_test.go
decisions:
  - "Concurrency gate is the 0010 partial unique index itself — the INSERT either wins or the loser surfaces ErrActiveManualRun (409)"
  - "Zero ceiling refused at the HTTP boundary as a typed 422; pause refusals carry the pause reason"
  - "reason is an audit-row annotation only (06-01 convention); validate() stays audit-agnostic, length enforced at the boundary"
  - "Pinned resolution reuse is verbatim (selected jsonb round-trip); the spawn gate never re-resolves approved plans for pinned runs"
  - "Manual/simulate runs keep report link + neutral check-run but never patch the deploy's sticky comment"
metrics:
  duration: ~3h (across resumed sessions)
  completed: 2026-09-23
  tasks: 3
  commits: 3
status: complete
---

# Phase 6 Plan 2: Manual Trigger Backend Summary

**One-liner:** Atomic manual-trigger transaction (pause → budget → pin → insert-as-gate → audit → River enqueue) with GitHub-head retest resolution and spawn-gate pin acceptance.

## What Was Built

- **T1 — Trigger service (`api/internal/cockpit/trigger.go`, `trigger_gates.go`, `trigger_http.go`)**: `Service.Trigger` validates the request, looks up the app, checks the global pause, evaluates the layered budget (zero ceiling → typed refusal), re-validates the plan pin (approved version of THIS app, parseable, non-zero steps), resolves the head (plain = request SHA; reproduce = source run SHA; retest = GitHub default-branch head, degraded GitHub → fail-closed), then in one tx inserts the manual run (synthetic `-nextval('cockpit_run_ids')` workflow_run_id, insert-as-gate via the 0010 partial index), stamps the pinned `run_plan_resolutions` row, appends the audit event, and enqueues the execute job on the runner queue. Wired in `main.go` with `GitHubHeads` resolver.
- **T2 — Migration regression (`api/internal/app/migrations/migrations_test.go`)**: `TestManualTriggerSource` proves the fail-closed ordering — pre-0010 constraint text rejects `trigger_source='manual'` (temp-schema replay of the 0007 constraint), post-0010 insert passes, the `cockpit_run_ids` sequence yields strictly decreasing negative ids, and the partial unique index admits same-app refusal as a unique violation while different apps proceed. Self-skips without `E2AI_TEST_DATABASE_URL`.
- **T3 — Downstream protection (`suite/execute.go`, `report/comment.go`)**: `resolvePlanGate` early-reuses a trigger-stamped approved resolution (mode `approved`) verbatim — a post-trigger approval change never displaces the pin. The report publisher carries `TriggerSource` on `runFacts` and gates the sticky-comment leg on `webhook`; manual/simulate runs still get the report link and a neutral check-run.

## Verification

- `go build ./...`, `go vet` clean across api module.
- `TestTriggerRequestValidation` (pure unit) PASSES; DB-backed tests (`TestTriggerZeroCeilingRefuses`, `TestTriggerPausedRefuses`, `TestTriggerRacingInsertAsGate` — one 202 / one 409, `TestTriggerInsertsManualRunAtProvisioning`, `TestManualTriggerSource`, `TestPlanGatePinnedStampSurvivesApprovalChange`) compile and self-skip without `E2AI_TEST_DATABASE_URL` per the harness convention; no postgres/docker was reachable on this machine.
- `TestPublishManualRunNeverTouchesStickyComment` PASSES against the stateful fake GitHub; full `internal/report` suite green.
- 3 commits measured from the plan ledger (0cde2d30 → 309bbca0).

## Deviations from Plan

None — plan executed as written. (One pre-existing condition noted: `page_test.go` / `trajectory_test.go` are un-gofmt'd from before this plan; untouched per scope boundary.)

## Self-Check: PASSED

- FOUND: api/internal/cockpit/{trigger,trigger_gates,trigger_http,trigger_test}.go
- FOUND: api/internal/app/migrations/migrations_test.go
- FOUND: commits deee57bb (T1), 29316e3c (T2), 309bbca0 (T3)

## Commits

- deee57bb — feat(06-02): manual trigger service — gates, pins, and insert-as-gate (T1)
- 29316e3c — test(06-02): migration constraint regression for manual trigger_source (T2)
- 309bbca0 — feat(06-02): pinned-run spawn gate reuse + manual runs never patch sticky comments (T3)
