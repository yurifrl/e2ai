# Phase 5 Research: Hardening — scout digests

Two scouts (P5Research — planning docs; P5Code — codebase map). Citations: (.planning/research/FILE §section) and code refs.

## Locked decisions that constrain phase 5 (report, do not re-litigate)

1. Report-only posture — heals/repairs are proposals; nothing gates merges.
2. Governance rides along (P1/P3 floors landed); P5 = flake policy + repair governance + multi-app.
3. aihub is the only gateway — per-role routing (`modelForRole`, `E2AI_AGENT_MODEL_<ROLE>`); no second gateway.
4. Refuse-to-heal gate: never rewrite below confidence floor (~0.7), never heal on error-state/alert pages, every heal = reviewable PR diff, escalation to humans. KPI = caught regressions, not healed tests.
5. Small fixed roster (Navigator, Vision Judge, Plan Author, Edge Hunter, Flake Analyst) with distinct prompts/budgets/permissions; handoffs through DB only.
6. Verifier independence is architectural: separate session/context, different model family for high-stakes; oracle = human-approved corpus_behaviors; no citation → INCONCLUSIVE, never PASS.
7. Retry discipline: never auto-retry APP_BUG; retries only infra-class (mobile: boot/screenshot-class only). Evidence-gated, never blind.
8. Queue+journal durability (River/Postgres), append-only run log.
9. Research posture: quarantine policy + Edge-Hunter bounds were the only open topics; both resolved below.

## Failure taxonomy (locked shape)

- Attribution classes: `{APP_BUG, AGENT_ERROR, INFRA_FLAKE, TEST_DRIFT, INCONCLUSIVE}` — exactly one per failure, no free-text.
- Verdict taxonomy (landed P1): `PASS/FAIL/FLAKY/INFRA_ERROR/INFRA_FLAKE/INCONCLUSIVE` from evidence + run state.
- Classification enforced in code around LLM judgment: confidence floor, error-state pages never auto-classified, low-confidence escalates.
- INFRA_FLAKE must be evidence-backed (boot times, adb/idb errors, step timings) — landed P4 (readiness probes, classify subtypes).
- Track classification accuracy itself (human spot-check) as a first-class metric.

## Flake governance (FLAK-01)

- Flake rates per test and per app from run history; test identity across runs = (file,title) — verdict.go notes this is the comparability key.
- Quarantine: quarantined tests keep running but are reported separately, not silently dropped; quarantine event records evidence + who/what + expiry.
- Alerting: flake rate is a tracked metric with an alert threshold, set empirically, not assumed.
- Mobile: retry only boot/screenshot-class failures (P4 INFRA_FLAKE plumbing feeds this).

## Selector repair (FLAK-02)

- Stale detection + repair proposals; approval-gated; only touches e2ai's own corpus; "suggesting a selector fix = allowed; rewriting app code = blocked" (Paperclip policy model: allowed / ask_first / off).
- Never heal below confidence floor; never on error pages; heals appear as reviewable diffs for human adoption.

## Trends (TREN-01)

- Verdict-flip rate: same suite ≥5× on unchanged PR; record rate; alert threshold empirical. LLM nondeterminism data: 5–15% accuracy variation at temp 0 (arXiv 2408.04667).
- Cost: layered defense (in-loop caps + stuck detection + gateway budgets + backstop); caps at 3–5× measured p95; budget rejections are the earliest runaway signal — log every one with run/app/agent/reason; alert on any run > k× p95.
- Run-level cost = aggregate agent_sessions.cost_millicents (+ session_steps granularity); suite compute cost out of scope unless trivially available.
- Model prices as versioned config, not code constants.

## MCP (MCP-01) — greenfield

- Purpose: coding agents in PRs query "what failed and why" over run history + evidence. No tool shapes specified anywhere — design freedom, but: read-only, reuse token-gated evidence links, mount/auth/error patterns from agents/http.go.
- Anti-goal: no second write surface; MCP never mutates the app under test nor e2ai state.

## Edge Hunter (EDGE-01) — bounds (the open research topic, resolved)

- Bounded scheduled sweeps, opt-in per app; unbounded exploration is an anti-feature (noisy findings, budget burn).
- Charter per sweep: bounded step ceiling from config (reuse plan step-ceiling machinery), fixed budget, scheduled interval, findings are learnings (corpus append → human promote), never auto-promoted.
- Sweep runs get their own run rows (trigger_source `sweep` — DB constraint check needed, only webhook|reconcile exist today) so reports/costs/trends treat them uniformly.

## Corpus health (CORP-03)

- Corpus maintainer records verified-once / flaky / stale per intent, feeding derivation; stale detection lands with selector repair (same trigger).
- Human-promote flow is delivered (P2); P5 adds health rollup + stale flags; no pipeline self-mutation.

## Multi-app (APPS-03)

- Registry row schema: repo, deploy workflow name, tag scheme, environments, surface list, target URLs, corpus root. Plumbing is delivered (multi-yaml seeding, per-app matchers, per-app surfaces/plans/corpus).
- Known inert piece: `webhook_secret_ref` column exists but `ingest.NewHandler` takes one shared secret — honor per-app refs (fall back to shared env when NULL) before app #2.
- Second app = registry yaml + config, zero code changes — prove with a template/example row (not a real betweb registration).
- Unknown app → quarantine event for admin triage, not a crash.

## Roster debt from phase 3 (AGNT-01/03/04) — design locked, implementation missing

- Navigation: a11y-tree grounding primary, locator-first (Playwright locators), Stagehand act() as NL fallback (dep declared, unused — package.json:19), vision verifies at decision points, never clicks blind; log pre-action a11y snapshot + locator with every act.
- Vision Judge: separate session (structural — Enqueue already spawns the pair), different gateway model family via role routing, oracle = corpus_behaviors (GetBehavior seam exists), structured JSON verdict schemas (compare parsed fields, 2-of-3 rerun for contested), deterministic side-effect assertions alongside VLM judgment ("the LLM narrates; code asserts").
- Plan Author: covered by deterministic PR-diff derivation (P2) + human-promoted learnings — NOT a separate LLM agent; agent-owned expectation writes are an anti-feature. Role stays in vocabulary for budget/permissions purposes.

## Codebase ground truth (P5Code)

- Migrations 0001–0006; next 0007; new tables MUST append to `appTables` drop list (app/postgres.go:28-34).
- Two River queues: `default` (api workers: wait-rollout, report, orphan sweep, canary enqueue), `runner` (execute, agent:session). Periodic-job recipe: stateless Args + Kind() + default-queue worker + interval const + main.go registration (exemplars: agents/sweep.go, suite/canary.go). Per-app opt-in reads DB, not env (canary's env gate is the anti-pattern for per-app scoping).
- Agent role wiring: Go side complete (RoleEdgeHunter/RoleFlakeAnalyst consts, worker.go RoleModels, runner env); TS `index.ts:109-126` role entry intentionally unwired; runLoop consumes deps.role as free string — additive, low risk.
- Sessions spawn only via admin Enqueue (navigator+judge pair) while run is provisioning/executing/reporting; sweeps need own runs + spawn-gate extension.
- Runs listing: single global/app-scoped query, no time-window/aggregation; no cost columns on runs (agent cost in agent_sessions/session_steps); no quarantine table; no MCP code; no flake-rate rollups.
- Wire contract report/page.go ↔ web/src/lib/types.ts is an exact mirror (every DTO edit is two-sided); report "HTML" is React-rendered from the JSON blob; i18n ×4 locales per page; manifest nav+commands schema-validated.
- ApprovalsQueuePage is the UI pattern for quarantine queue; corpus promote/dismiss actions already wired.
