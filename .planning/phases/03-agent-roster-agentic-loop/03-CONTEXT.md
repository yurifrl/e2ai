# Phase 3: Agent roster and the agentic loop - Context

**Phase Goal:** The product's core claim goes live: five governed agents drive the web loop — Navigator clicks, Vision Judge verifies — with independent context, hard budgets, and a zero-credential sandbox.

## Aligned Requirements
AGNT-01..05, BUDG-01..02, SAFE-01, REPT-04

## Specifics (auto — discuss skipped)

User directive: run autonomously, go with suggestions. Decisions from research + sketch direction:
- **Roster (AGNT-01):** five roles — Navigator, Vision Judge, Plan Author, Edge-case Hunter, Flakiness Analyst. Roles are queue-worker kinds with governed handoffs (session log rows), not free-form chat.
- **Orchestrator (AGNT-02):** River-queue session orchestration: session = DB row with heartbeat; atomic claiming via `select ... for update skip locked`; append-only session_steps log; orphan recovery sweep re-queues sessions whose heartbeat lapsed.
- **Navigator (AGNT-03):** Playwright + Stagehand, a11y-tree-grounded locator-first navigation; never clicks blind — every action cites a locator + pre-action a11y snapshot.
- **Vision Judge (AGNT-04):** separate agent with independent context; different model family than Navigator for high-stakes verdicts; oracle = corpus_behaviors (Phase 2 store); deterministic side-effect assertions (URL, DOM state, HTTP) run alongside VLM judgment; verdict without machine-checkable evidence = INCONCLUSIVE.
- **aihub gateway (AGNT-05):** ALL model traffic via the aihub gateway (OpenAI-compatible API); no provider keys in agent code; gateway key from op-injected env only.
- **Budgets (BUDG-01/02):** step ceiling + per-run cost accumulator; BudgetExhausted is non-retryable; gateway-level budget keys stop runaway spend.
- **Safety (SAFE-01):** operator kill switch (admin surface), stuck-detection (heartbeat lapse), zero-credential egress-allowlisted staging-only sandbox, full trajectory logging.
- **Reports (REPT-04):** agentic run reports carry trajectory + per-step evidence citations; no machine-checkable evidence -> INCONCLUSIVE (extends Phase 1 report).

## Boundaries
- Web only. No mobile (Phase 4). No flake governance/trends (Phase 5).
- Agents execute approved plans + bounded hunts; never merge-gate; never production.
- Model choices follow .planning/research/STACK.md aihub gateway facts; fallbacks explicit.

## Deferred
None.