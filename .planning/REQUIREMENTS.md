# Requirements: E2AI — the Always-On AI QA Arm

**Defined:** 2026-09-19
**Core Value:** An arm of testers always reporting live, never blocking: every deploy produces rich, explained, evidence-backed QA reports while growing a trustworthy database of expected behavior.

## v1 Requirements

Requirements for initial release. Each maps to a roadmap phase (P1–P5 per research SUMMARY). All are report-only: nothing here gates a merge, deploy, or CI check.

### Module & Integration

- [x] **INTG-01** *(P1)*: Module skeleton registers into the shared admin shell via `admin.module.json` (id `@admin/e2ai`, routeBase `/e2ai`, nav sections, commands, permissions) following the incidents-admin family pattern
- [x] **INTG-02** *(P1)*: Go hexagonal API + Postgres + River queue (Connect-RPC via BFF), local dev stack via Tilt with mock auth — matches shared-admin conventions
- [x] **INTG-03** *(P1)*: Report page renders inside the admin shell with live run state (start from a run, watch it progress, land on the full report)
### App Registry

- [x] **APPS-01** *(P1)*: Per-app config-as-code registration: repo, deploy workflow name, surfaces, environments, plans source, corpus root
- [x] **APPS-02** *(P1)*: bet-app registered as the first app — web surface, staging environment
- [x] **APPS-03** *(P5)*: Multi-app onboarding scaffold: adding a new app is a registry row + config, no code changes (`adminx`-style)

### Deploy Ingest

- [x] **INGE-01** *(P1)*: `workflow_run` deploy webhook ingest: HMAC validation, idempotent by delivery GUID + `repo/workflow/run_id`, 202-then-enqueue (no inline processing)
- [x] **INGE-02** *(P1)*: Poll-based reconciliation sweep for missed/duplicate webhook deliveries (webhooks are not auto-redelivered)
- [x] **INGE-03** *(P1)*: Deploy-completion ≠ live: rollout-health polling determines when QA actually starts against the deployed build
### Deterministic Suite (P1)

- [x] **SUIT-01** *(P1)*: Seeded suite runs the existing bet-client Playwright specs on staging per deploy; run records consume Playwright blob/json + traces as evidence
- [x] **SUIT-02** *(P1)*: Deterministic runs establish the baseline the agentic layer must later beat
### Verdicts & Reports

- [x] **VERD-01** *(P1)*: Verdict engine with fixed taxonomy `PASS / FAIL / FLAKY / INFRA_ERROR / INCONCLUSIVE`; verdicts derive from evidence + run state, never exit codes alone
- [x] **REPT-01** *(P1)*: Live HTML reports with screenshots and plain-language explanations (annotated screenshot canvas as chosen design; filmstrip = step timeline)
- [x] **REPT-02** *(P1)*: Failure classification answers one question per failure: app bug, test/expectation gap, agent flake, or infra

- [x] **REPT-03** *(P1)*: Sticky PR comment (one, updated per run) + neutral status check — the check is informational, never a failing gate
- [x] **REPT-04** *(P3)*: Agentic run reports carry agent trajectory, per-step evidence citations, and verdicts that cite machine-checkable evidence or render `INCONCLUSIVE`

- [x] **SECR-01** *(P1)*: Secrets hygiene: credentials kept out of screenshots, logs, fixture metadata, and reports
### Test Plans & Approvals

- [x] **PLAN-01** *(P2)*: Declarative markdown plan format with frontmatter (app, surfaces, behaviors, assertions) — the format spec from sketch 006 is the contract
- [x] **PLAN-02** *(P2)*: Plans sourced per-app via registry config: app repo path or central plans repo, both supported
- [x] **APPR-01** *(P2)*: Approval queue: new and changed plans wait for human approval; only previously approved plans run unattended on deploys
- [x] **APPR-02** *(P2)*: Approval UI = rendered plan + raw diff toggle (chosen design, sketch 003-B); approvals are attributable (who, when, which version)
### Test Derivation

- [x] **DERV-01** *(P2)*: PR diff → affected-scope mapping → run plan (subset + prioritization of the deterministic suite)
- [x] **DERV-02** *(P2)*: Plan Author agent drafts new intent-level checks from the diff; drafts queue for human approval before ever running
- [x] **DERV-03** *(P2)*: Tautology guard: derived expected values come from independent sources (specs, tickets, recorded prior behavior) — never from re-reading the diff's own implementation
### Behavior Corpus

- [x] **CORP-01** *(P2)*: Expected-behavior store seeded from existing Playwright specs — first cut of the verdict oracle
- [x] **CORP-02** *(P2)*: Agent observations auto-capture as unverified learnings; humans promote to verified baselines with provenance — no self-mutating corpus
- [x] **CORP-03** *(P5)*: Corpus-health page: coverage of the app's critical paths, stale behaviors, promotion queue

### Agent Roster & Run Loop

- [x] **AGNT-01** *(P3)*: Fixed roster with governed handoffs: Navigator (driver), Vision Judge (verifier), Plan Author (authoring), Edge-case Hunter (bounded exploration), Flakiness Analyst (stability)
- [x] **AGNT-02** *(P3)*: Session orchestrator: River queue, heartbeat/wake, append-only run log, atomic run claiming, orphan recovery
- [x] **AGNT-03** *(P3)*: Navigator: a11y-tree-grounded locator-first navigation on web (Playwright + Stagehand), vision used to verify — not to click blind
- [x] **AGNT-04** *(P3)*: Vision Judge: separate agent, independent context (different model family for high-stakes verdicts), oracle = expected-behavior store; deterministic side-effect assertions alongside VLM judgment
- [x] **AGNT-05** *(P3)*: All model traffic routes through the company aihub gateway; no provider keys in agents

### Budgets & Safety

- [x] **BUDG-01** *(P3)*: Layered budget caps: step ceiling, per-run cost accumulator with non-retryable `BudgetExhausted`, gateway-level per-run budget keys
- [x] **BUDG-02** *(P3)*: Operator kill switch stops runs without a deploy; stuck detection over action history
- [x] **SAFE-01** *(P3)*: Agent sandbox: zero credentials, egress allowlist, action allowlists, staging-only execution, full trajectory logging for forensics (page content is data, never instructions)

### Surfaces

- [x] **PLAT-01** *(P1)*: Web surface runs through Playwright with viewport projects matching bet-client's config (desktop + mobile viewports)
- [x] **PLAT-02** *(P4)*: Android emulator surface via Maestro (YAML flows, native screenshots/video)
- [x] **PLAT-03** *(P4)*: iOS simulator surface: `simctl` lifecycle + idb primitives for grounding, Maestro as input/compat layer
- [x] **PLAT-04** *(P4)*: WebView-aware mobile loops: readiness probes, animation/network/render-idle before capture (double-lag handling); boot/screenshot failures auto-classified `INFRA_FLAKE`
- [x] **PLAT-05** *(P4)*: Version-pinned runner images (Xcode/emulators) with deliberate upgrades behind canary runs

### Flake Governance & Trends

- [x] **FLAK-01** *(P5)*: Flakiness Analyst: run-history flake rates per test/app, evidence-gated retries (never blind re-runs of `APP_BUG`), quarantine
- [x] **FLAK-02** *(P5)*: Stale detection + selector-repair proposals as human-reviewable diffs — refuse-to-heal gate (never heals below confidence floor, never on error pages)
- [x] **TREN-01** *(P5)*: Trends dashboard: verdict-flip rate as alerting metric, run history, cost per run/agent
- [x] **MCP-01** *(P5)*: MCP surface for coding agents: query runs, failures, and evidence from a PR session
- [x] **EDGE-01** *(P5)*: Edge-case Hunter formalized with bounded scheduled sweeps (opt-in per app) — unbounded exploration stays an anti-feature

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
- v1.0 requirements: 45 total
- Mapped to phases: 45
- Unmapped: 0 ✓

## v1.1 Requirements — Admin Cockpit

Defined 2026-09-22 from research (STACK/FEATURES/ARCHITECTURE/PITFALLS/SUMMARY). Every requirement is cockpit-side: the arm is actuated, the app under test is never mutated; approval gating and budget discipline hold everywhere.

### Command Center

- [ ] **COCK-01** *(P6)*: Admin landing is the Command Center at the module root (sketch 001-A built for real): KPI header (runs today, pass rate today, pending approvals badge, budget spend vs cap, paused/kill-switch state), reverse-chron live feed, agent rail
- [ ] **COCK-02** *(P6)*: Feed rows expand inline to a compact detail strip (verdict, duration, cost, evidence thumbnails); app filter chips constrain the feed; notable arm events (session failures, flakes quarantined, triage arrivals) appear as feed events
- [ ] **COCK-03** *(P6)*: Agent rail shows live agent sessions (role, status, heartbeat age) with stuck sessions (>90s lapsed heartbeat, API-computed flag) flagged and linking to Run Detail
- [ ] **COCK-04** *(P6)*: Global pause control lives in the KPI header: paused state blocks all new triggers with an explicit reason; current state visible at all times

### Manual Trigger

- [ ] **TRIG-01** *(P6)*: Admin can trigger a run without a deploy from the cockpit, registry card, plan page, or run re-run — creating a run with `trigger_source = 'manual'` (migration widens the check constraint), stamped with operator identity and optional reason, bypassing wait-rollout (app already deployed)
- [ ] **TRIG-02** *(P6)*: Trigger picker lists only approved plan versions; draft/unapproved versions render disabled with a one-line reason; an app with zero approved versions cannot trigger and links to its Approvals-queue plans
- [ ] **TRIG-03** *(P6)*: Trigger confirm shows the resolved plan identity (version, source PR/SHA), current budget state for the app (spend vs layered caps), and run shape; triggering is disabled with an explicit reason when budget is exhausted or the arm is paused — never silently queued
- [ ] **TRIG-04** *(P6)*: Re-run always creates a new run row (append-only): "same commit (reproduce)" pins the original head SHA, "latest head (retest)" pulls current default branch; a manual run-retry is never a River job retry of the original execute job
- [ ] **TRIG-05** *(P6)*: Manual runs never collide with the `(app_id, workflow_run_id)` webhook-dedupe key — synthetic ids come from a dedicated sequence (fail-closed regression test on uniqueness)

### Webhook Inbox

- [ ] **INBX-01** *(P7)*: Deliveries page lists webhook events reverse-chron with app badge, signature badge, and ingest-outcome chip (ingested→run link, duplicate, redelivered, triaged, rejected); row expand shows parsed metadata, raw JSON payload viewer, and links (View run, Open in GitHub)
- [ ] **INBX-02** *(P7)*: Per-delivery redeliver action calls GitHub's redeliver API with a consequence sentence and cooldown; re-ingest flows the same idempotent dedupe path (duplicate = harmless, recorded)
- [ ] **INBX-03** *(P7)*: UNCLAIMED triage rows are first-class inbox rows: "belongs to no registered app" + register-app deep link pre-filling owner/repo/workflow from the payload; on save the triage row resolves and past unclaimed deliveries for that repo show "now claimed" (no retroactive re-ingest)

### Admin Actions

- [ ] **ACTN-01** *(P6)*: Simulate webhook from the cockpit: pick app + payload shape; the api server renders and HMAC-signs with the app's resolved secret (secrets never reach the browser); result shows ingest outcome + created run link
- [ ] **ACTN-02** *(P8)*: Run Detail "Add agent session" with role picker, enabled only while the run is executing, surfacing the approved-plan gate inline and the budget it will consume
- [ ] **ACTN-03** *(P8)*: Kill action on agent-session rows (Run Detail + agent rail) with reason field landing in the append-only error note; the API's 409 race semantics ("another actor finalized the session") surface verbatim
- [ ] **ACTN-04** *(P8)*: Plans-sync is one click on the Plans page header and app cards, showing the sync result (plans added/updated)
- [ ] **ACTN-05** *(P8)*: Cockpit utility actions: canary run-now, MCP probe (read-only tools/list + one bounded call, shows latency + raw response, no budget), force orphan sweep
- [ ] **ACTN-06** *(P8)*: Every mutating action follows the same contract: consequence sentence before commit, confirm only where destructive, server-ack waits (no optimistic UI), API errors verbatim, and already-in-UI actions (approve/reject, quarantine, corpus promote, alert acknowledge) must not regress

### App Registry

- [ ] **APPS-04** *(P9)*: Registry cards page: per-app cards with health summary (runs today, last verdict, agent mini-rail), config state (edge-sweep, thresholds, quarantine/triage counts), detail view
- [ ] **APPS-05** *(P9)*: Add/edit app wizard: registration fields + flake thresholds + edge-sweep toggle; secrets stored as 1Password references only — never raw values; server verifies the ref resolves before save; duplicate repo/workflow/environment combination surfaces the unique-constraint error inline
- [ ] **APPS-06** *(P9)*: App delete is a two-step typed-slug confirm with dependency warning; deletion keeps run history and only stops registration + ingest matching
- [ ] **APPS-07** *(P9)*: Registry CRUD is gated by the `admin.e2ai.admin` permission — its first real consumer, with the BFF→header→permission chain verified in staging

### Jobs Observatory

- [ ] **JOBS-01** *(P9)*: Jobs page lists River jobs with state/queue/kind filters and cursor pagination, plus a canary card (inert vs active, pinned image, last canary verdict)
- [ ] **JOBS-02** *(P9)*: Job cancel pairs the River cancel with a guarded run-state transition and error note; job retry is offered only for default-queue kinds (report, plans sync, derive draft) — run-level retry routes to the manual re-run flow (TRIG-04)
- [ ] **JOBS-03** *(P9)*: Queue pause/resume behind admin permission with a persistent pause banner; pause state survives restarts and is reflected in the Command Center KPI header

### Audit & Budget Integrity

- [ ] **AUDT-01** *(P6)*: Every cockpit mutation attributes operator identity + timestamp in the affected row/log so "who triggered/killed/replayed this" is answerable from the UI
- [ ] **BUDG-03** *(P6)*: Trigger-time budget gate: validates both ceilings strictly positive (ceiling 0 is never unlimited), aggregates spend per app/day against caps, and serializes concurrent triggers per run (no double-paid sessions from the racy read-then-insert path)

### Out of Scope (v1.1 anti-features)

- Mutating the app under test from the cockpit (deploys, restarts, env changes) — link out instead; report-only invariant
- Auto-approving plans from the cockpit — Approvals queue stays the only approval path
- Raw secret entry/display in any form — vault-reference resolution only
- Free-form SQL/job console — curated per-job actions with fixed semantics
- Rework of v1.0 pages (Runs/Plans/Corpus/Quality) — new actions wire into existing pages
- Dedicated audit-log viewer page (audit writes land in v1.1; viewer can follow)

### Future

- Bulk redelivery; per-app config editing beyond thresholds/toggles (workflow/env changes are rare); dedicated audit-log page; corpus-related cockpit views

## v1.1 Coverage

| Area | IDs | Phase |
|------|-----|-------|
| Command Center | COCK-01..04 | 6 |
| Manual Trigger | TRIG-01..05 | 6 |
| Webhook Inbox | INBX-01..03 | 7 |
| Webhook Simulate | ACTN-01 | 6 |
| Admin Actions | ACTN-02..06 | 8 |
| App Registry | APPS-04..07 | 9 |
| Jobs Observatory | JOBS-01..03 | 9 |
| Audit & Budget | AUDT-01, BUDG-03 | 6 |

---
*Requirements defined: 2026-09-19; v1.1 section added 2026-09-22*
