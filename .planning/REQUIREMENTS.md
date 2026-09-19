# Requirements: E2AI — the Always-On AI QA Arm

**Defined:** 2026-09-19
**Core Value:** An arm of testers always reporting live, never blocking: every deploy produces rich, explained, evidence-backed QA reports while growing a trustworthy database of expected behavior.

## v1 Requirements

Requirements for initial release. Each maps to a roadmap phase (P1–P5 per research SUMMARY). All are report-only: nothing here gates a merge, deploy, or CI check.

### Module & Integration

- [ ] **INTG-01** *(P1)*: Module skeleton registers into the shared admin shell via `admin.module.json` (id `@admin/e2ai`, routeBase `/e2ai`, nav sections, commands, permissions) following the incidents-admin family pattern
- [ ] **INTG-02** *(P1)*: Go hexagonal API + Postgres + River queue (Connect-RPC via BFF), local dev stack via Tilt with mock auth — matches shared-admin conventions
- [ ] **INTG-03** *(P1)*: Report page renders inside the admin shell with live run state (start from a run, watch it progress, land on the full report)

### App Registry

- [ ] **APPS-01** *(P1)*: Per-app config-as-code registration: repo, deploy workflow name, surfaces, environments, plans source, corpus root
- [ ] **APPS-02** *(P1)*: bet-app registered as the first app — web surface, staging environment
- [ ] **APPS-03** *(P5)*: Multi-app onboarding scaffold: adding a new app is a registry row + config, no code changes (`adminx`-style)

### Deploy Ingest

- [ ] **INGE-01** *(P1)*: `workflow_run` deploy webhook ingest: HMAC validation, idempotent by delivery GUID + `repo/workflow/run_id`, 202-then-enqueue (no inline processing)
- [ ] **INGE-02** *(P1)*: Poll-based reconciliation sweep for missed/duplicate webhook deliveries (webhooks are not auto-redelivered)
- [ ] **INGE-03** *(P1)*: Deploy-completion ≠ live: rollout-health polling determines when QA actually starts against the deployed build

### Deterministic Suite (P1)

- [ ] **SUIT-01** *(P1)*: Seeded suite runs the existing bet-client Playwright specs on staging per deploy; run records consume Playwright blob/json + traces as evidence
- [ ] **SUIT-02** *(P1)*: Deterministic runs establish the baseline the agentic layer must later beat

### Verdicts & Reports

- [ ] **VERD-01** *(P1)*: Verdict engine with fixed taxonomy `PASS / FAIL / FLAKY / INFRA_ERROR / INCONCLUSIVE`; verdicts derive from evidence + run state, never exit codes alone
- [ ] **REPT-01** *(P1)*: Live HTML reports with screenshots and plain-language explanations (annotated screenshot canvas as chosen design; filmstrip = step timeline)
- [ ] **REPT-02** *(P1)*: Failure classification answers one question per failure: app bug, test/expectation gap, agent flake, or infra
- [ ] **REPT-03** *(P1)*: Sticky PR comment (one, updated per run) + neutral status check — the check is informational, never a failing gate
- [ ] **REPT-04** *(P3)*: Agentic run reports carry agent trajectory, per-step evidence citations, and verdicts that cite machine-checkable evidence or render `INCONCLUSIVE`
- [ ] **SECR-01** *(P1)*: Secrets hygiene: credentials kept out of screenshots, logs, fixture metadata, and reports

### Test Plans & Approvals

- [ ] **PLAN-01** *(P2)*: Declarative markdown plan format with frontmatter (app, surfaces, behaviors, assertions) — the format spec from sketch 006 is the contract
- [ ] **PLAN-02** *(P2)*: Plans sourced per-app via registry config: app repo path or central plans repo, both supported
- [ ] **APPR-01** *(P2)*: Approval queue: new and changed plans wait for human approval; only previously approved plans run unattended on deploys
- [ ] **APPR-02** *(P2)*: Approval UI = rendered plan + raw diff toggle (chosen design, sketch 003-B); approvals are attributable (who, when, which version)

### Test Derivation

- [ ] **DERV-01** *(P2)*: PR diff → affected-scope mapping → run plan (subset + prioritization of the deterministic suite)
- [ ] **DERV-02** *(P2)*: Plan Author agent drafts new intent-level checks from the diff; drafts queue for human approval before ever running
- [ ] **DERV-03** *(P2)*: Tautology guard: derived expected values come from independent sources (specs, tickets, recorded prior behavior) — never from re-reading the diff's own implementation

### Behavior Corpus

- [ ] **CORP-01** *(P2)*: Expected-behavior store seeded from existing Playwright specs — first cut of the verdict oracle
- [ ] **CORP-02** *(P2)*: Agent observations auto-capture as unverified learnings; humans promote to verified baselines with provenance — no self-mutating corpus
- [ ] **CORP-03** *(P5)*: Corpus-health page: coverage of the app's critical paths, stale behaviors, promotion queue

### Agent Roster & Run Loop

- [ ] **AGNT-01** *(P3)*: Fixed roster with governed handoffs: Navigator (driver), Vision Judge (verifier), Plan Author (authoring), Edge-case Hunter (bounded exploration), Flakiness Analyst (stability)
- [ ] **AGNT-02** *(P3)*: Session orchestrator: River queue, heartbeat/wake, append-only run log, atomic run claiming, orphan recovery
- [ ] **AGNT-03** *(P3)*: Navigator: a11y-tree-grounded locator-first navigation on web (Playwright + Stagehand), vision used to verify — not to click blind
- [ ] **AGNT-04** *(P3)*: Vision Judge: separate agent, independent context (different model family for high-stakes verdicts), oracle = expected-behavior store; deterministic side-effect assertions alongside VLM judgment
- [ ] **AGNT-05** *(P3)*: All model traffic routes through the company aihub gateway; no provider keys in agents

### Budgets & Safety

- [ ] **BUDG-01** *(P3)*: Layered budget caps: step ceiling, per-run cost accumulator with non-retryable `BudgetExhausted`, gateway-level per-run budget keys
- [ ] **BUDG-02** *(P3)*: Operator kill switch stops runs without a deploy; stuck detection over action history
- [ ] **SAFE-01** *(P3)*: Agent sandbox: zero credentials, egress allowlist, action allowlists, staging-only execution, full trajectory logging for forensics (page content is data, never instructions)

### Surfaces

- [ ] **PLAT-01** *(P1)*: Web surface runs through Playwright with viewport projects matching bet-client's config (desktop + mobile viewports)
- [ ] **PLAT-02** *(P4)*: Android emulator surface via Maestro (YAML flows, native screenshots/video)
- [ ] **PLAT-03** *(P4)*: iOS simulator surface: `simctl` lifecycle + idb primitives for grounding, Maestro as input/compat layer
- [ ] **PLAT-04** *(P4)*: WebView-aware mobile loops: readiness probes, animation/network/render-idle before capture (double-lag handling); boot/screenshot failures auto-classified `INFRA_FLAKE`
- [ ] **PLAT-05** *(P4)*: Version-pinned runner images (Xcode/emulators) with deliberate upgrades behind canary runs

### Flake Governance & Trends

- [ ] **FLAK-01** *(P5)*: Flakiness Analyst: run-history flake rates per test/app, evidence-gated retries (never blind re-runs of `APP_BUG`), quarantine
- [ ] **FLAK-02** *(P5)*: Stale detection + selector-repair proposals as human-reviewable diffs — refuse-to-heal gate (never heals below confidence floor, never on error pages)
- [ ] **TREN-01** *(P5)*: Trends dashboard: verdict-flip rate as alerting metric, run history, cost per run/agent
- [ ] **MCP-01** *(P5)*: MCP surface for coding agents: query runs, failures, and evidence from a PR session
- [ ] **EDGE-01** *(P5)*: Edge-case Hunter formalized with bounded scheduled sweeps (opt-in per app) — unbounded exploration stays an anti-feature

## Coverage

| Area | IDs | Phase |
|------|-----|-------|
| Module & Integration | INTG-01..03 | 1 |
| App Registry | APPS-01..03 | 1, 5 |
| Deploy Ingest | INGE-01..03 | 1 |
| Deterministic Suite | SUIT-01..02 | 1 |
| Verdicts & Reports | VERD-01, REPT-01..04, SECR-01 | 1, 3 |
| Test Plans & Approvals | PLAN-01..02, APPR-01..02 | 2 |
| Test Derivation | DERV-01..03 | 2 |
| Behavior Corpus | CORP-01..03 | 2, 5 |
| Agent Roster & Run Loop | AGNT-01..05 | 3 |
| Budgets & Safety | BUDG-01..02, SAFE-01 | 3 |
| Surfaces | PLAT-01..05 | 1, 4 |
| Flake Governance & Trends | FLAK-01..02, TREN-01, MCP-01, EDGE-01 | 5 |

**Coverage:**
- v1 requirements: 40 total
- Mapped to phases: 40
- Unmapped: 0 ✓

---
*Requirements defined: 2026-09-19*
*Last updated: 2026-09-19 after initial definition*
