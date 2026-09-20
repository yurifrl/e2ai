# Project Roadmap: E2AI — the Always-On AI QA Arm

**Generated:** 2026-09-19
**Project:** @admin/e2ai (module `@admin/e2ai`, routeBase `/e2ai`)
**Requirements:** .planning/REQUIREMENTS.md — 45 requirements, 12 areas, 100% phase-mapped
**Invariant:** report-only everywhere — nothing in this roadmap gates a merge, deploy, or CI check

## Project Phases

### Phase 1: Web happy path: deterministic foundation
**Goal:** The full report-only QA loop for bet-app web on every deploy: webhook → run → deterministic Playwright suite → verdicts → live HTML report + sticky PR comment.

**Delivers:**
- Admin-shell module skeleton (INTG-01..03): `admin.module.json` registration, Go hexagonal API + Postgres + River via Tilt with mock auth, report page with live run state
- Deploy ingest (INGE-01..03): HMAC-validated idempotent `workflow_run` ingest (202-then-enqueue), reconciliation sweep for missed deliveries, rollout-health gating before QA starts
- Deterministic suite (SUIT-01..02): existing bet-client Playwright specs run on staging per deploy; blob/json + traces consumed as evidence; baseline for what agents must later beat
- Verdict engine + reports (VERD-01, REPT-01..03, SECR-01): fixed taxonomy `PASS/FAIL/FLAKY/INFRA_ERROR/INCONCLUSIVE`, annotated-screenshot live reports, failure classification, sticky PR comment + neutral check, secrets hygiene
- Web surface (PLAT-01): viewport projects matching bet-client config
- App registry (APPS-01..02): config-as-code registration, bet-app as first app

**Requirements covered:** INTG-01..03, APPS-01..02, INGE-01..03, SUIT-01..02, VERD-01, REPT-01..03, SECR-01, PLAT-01 (16/45)

**Success criteria:**
- Push to staging → webhook lands → deterministic run executes → report page shows screenshots + explanations → PR comment sticks and updates
- Replayed duplicate webhook delivery produces exactly one run (idempotency)
- QA starts only after rollout health confirms the build is live
- Every verdict traceable to evidence; no exit-code-only verdicts

**Depends on:** bet-client deploy workflow + staging environment (external, exists today)
**Guardrails:** No agents yet — Phase 1 is 100% deterministic. No mobile. No plans/approvals yet.

### Phase 2: Plans, approvals, and test derivation
**Goal:** Human-supervised autonomy begins: declarative test plans with an approval queue, PR-diff-driven test selection and plan drafting, and the behavior-corpus oracle seeded from existing specs.

**Delivers:**
- Plan format + sourcing (PLAN-01..02): declarative markdown with frontmatter (sketch-006 contract), per-app registry sourcing (app repo path or central plans repo)
- Approval queue (APPR-01..02): new/changed plans wait for human approval; approved plans run unattended on deploys; rendered-plan + raw-diff approval UI; attributable approvals
- Test derivation (DERV-01..03): PR diff → affected scope → run plan; Plan Author drafts intent-level checks queued for approval; tautology guard (expected values from independent sources only)
- Corpus seed (CORP-01..02): expected-behavior store seeded from Playwright specs; agent observations captured as unverified learnings with human promotion

**Requirements covered:** PLAN-01..02, APPR-01..02, DERV-01..03, CORP-01..02 (9/45)

**Success criteria:**
- A plan change triggers the approval queue; approved version runs unattended on next deploy
- A PR touching known scope gets a right-sized run plan (subset, not full suite)
- A drafted plan referencing only the diff's own implementation is rejected by the tautology guard
- Corpus entries carry provenance; no entry self-mutates without human promotion

**Depends on:** Phase 1 (registry, runs, verdicts exist)
**Guardrails:** Still deterministic + plans only — no agentic execution yet. Derivation drafts, never auto-runs.

### Phase 3: Agent roster and the agentic loop
**Goal:** The product's core claim goes live: five governed agents drive the web loop — Navigator clicks, Vision Judge verifies — with independent context, hard budgets, and a zero-credential sandbox.

**Delivers:**
- Roster + orchestrator (AGNT-01..02): Navigator, Vision Judge, Plan Author, Edge-case Hunter, Flakiness Analyst with governed handoffs; River-queue session orchestration with heartbeat/wake, append-only run log, atomic claiming, orphan recovery
- Navigator (AGNT-03): a11y-tree-grounded locator-first navigation (Playwright + Stagehand); vision verifies, never clicks blind
- Vision Judge (AGNT-04): separate agent, independent context (different model family for high-stakes verdicts); oracle = expected-behavior store; deterministic side-effect assertions alongside VLM judgment
- Evidence-grade agentic reports (REPT-04): trajectory + per-step citations; no machine-checkable evidence → `INCONCLUSIVE`
- Budgets & safety (BUDG-01..02, SAFE-01): step ceiling, per-run cost accumulator with non-retryable `BudgetExhausted`, gateway-level budget keys; operator kill switch + stuck detection; zero-credential egress-allowlisted staging-only sandbox; full trajectory logging
- aihub gateway (AGNT-05): all model traffic through company gateway, no provider keys in agents

**Requirements covered:** AGNT-01..05, BUDG-01..02, SAFE-01, REPT-04 (9/45)

**Success criteria:**
- An agentic run drives bet-app login + one payment flow on staging and produces an evidence-cited report alongside the deterministic run
- Vision Judge verdicts that lack machine-checkable evidence render `INCONCLUSIVE`, never confident guesswork
- A runaway run hits `BudgetExhausted` and stops — non-retryable, report says so
- Sandbox: no credentials reachable from agent context; egress beyond allowlist blocked and logged
- Kill switch stops a mid-flight run without any deploy involved

**Depends on:** Phase 1 (runs, verdicts, reports) + Phase 2 (corpus oracle, approved plans)
**Guardrails:** Web only. Agents execute approved plans + bounded hunts; never merge-gate; never touch production.

### Phase 4: Mobile surfaces
**Goal:** The same loop reaches the Android emulator and iOS simulator: Maestro-first Android, simctl+idb-grounded iOS, WebView-aware capture, pinned runner images.

**Delivers:**
- Android (PLAT-02): Maestro YAML flows with native screenshots/video
- iOS (PLAT-03): `simctl` lifecycle + idb grounding primitives, Maestro as input/compat layer
- WebView-aware loops (PLAT-04): readiness probes, animation/network/render-idle before capture, double-lag handling; boot/screenshot failures auto-classified `INFRA_FLAKE`
- Runner discipline (PLAT-05): version-pinned Xcode/emulator images, deliberate upgrades behind canary runs

**Requirements covered:** PLAT-02..05 (4/45)

**Success criteria:**
- An approved plan with mobile surfaces executes on emulator + simulator per deploy; reports carry native screenshots/video
- A webview screen mid-animation captures only after readiness probes pass (no double-lag screenshots in reports)
- Emulator cold-boot failure lands as `INFRA_FLAKE` with the boot log attached, not `FAIL`
- Runner image upgrade can be canaried on one run before fleet-wide rollout

**Depends on:** Phase 3 (agentic loop + Navigator surface abstraction extends to mobile)
**Guardrails:** Real devices (TestFlight/cloud device farms) out of scope for v1.

### Phase 5: Flake governance, trends, and multi-app scale
**Goal:** Make it trustworthy at scale: flake rates and evidence-gated retries, refuse-to-heal selector repair, trends + cost dashboards, MCP surface for coding agents, bounded edge sweeps, multi-app onboarding.

**Delivers:**
- Flakiness governance (FLAK-01..02): per-test/app flake rates from run history, evidence-gated retries (never blind re-runs of `APP_BUG`), quarantine; stale detection + selector-repair proposals as reviewable diffs behind the refuse-to-heal gate
- Trends (TREN-01): verdict-flip rate as alerting metric, run history, cost per run/agent
- MCP surface (MCP-01): coding agents query runs, failures, evidence from a PR session
- Bounded exploration (EDGE-01): Edge-case Hunter scheduled sweeps, opt-in per app; unbounded exploration stays an anti-feature
- Corpus health (CORP-03): coverage of critical paths, stale behaviors, promotion queue
- Multi-app (APPS-03): new app = registry row + config

**Requirements covered:** FLAK-01..02, TREN-01, MCP-01, EDGE-01, CORP-03, APPS-03 (7/45)

**Success criteria:**
- A test with >20% flake rate across recent runs is flagged and can be quarantined from the report denominator
- A broken selector produces a reviewable repair diff only above the confidence floor — never auto-applied, never on error pages
- Verdict-flip spike surfaces on the trends dashboard before anyone asks
- A second app registers via config only and gets its first deterministic run end-to-end
- A coding agent in a PR session queries e2ai via MCP and links to evidence

**Depends on:** Phase 3 (agent roster) + accumulated run history from Phases 1–3
**Guardrails:** No auto-healing below the confidence floor. Edge sweeps opt-in, bounded.

## Roadmap-Level Notes

- **Order is deliberate:** deterministic spine first (P1), human-supervised autonomy second (P2), agentic execution third (P3) — agents must beat the deterministic baseline and inherit verdict/report plumbing, not bootstrap their own. Mobile (P4) only after the web loop proves out; governance (P5) needs accumulated history to be meaningful.
- **Every phase ships reports:** the report-only invariant holds from Phase 1 commit one.
- **Approval gating scales with autonomy:** Phase 2 gates plans before agents exist; Phase 3 consumes that gate as its first safety rail.
- **Budgets land with agents, not after:** Phase 3 is unsafe without layered caps and the kill switch.

## Verification Approach

- Phase-level: gsd-verifier goal-backward check against the success criteria above; UI phases via live-DOM verification
- Cross-phase: integration check after Phase 3 (deterministic + agentic runs coexist on the same report surface) and after Phase 5 (second app + MCP consumer)

---
*Roadmap generated: 2026-09-19*
*Aligned with: SUMMARY.md 5-phase skeleton, REQUIREMENTS.md coverage (45/45 mapped)*
