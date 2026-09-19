# Project Research Summary

**Project:** E2AI — the Always-On AI QA Arm (`@admin/e2ai`)
**Domain:** Agentic, vision-driven autonomous E2E QA platform (report-only) spanning web + iOS simulator + Android emulator
**Researched:** 2026-09-19
**Confidence:** MEDIUM-HIGH (architecture and admin-platform fit are code-grounded HIGH; agentic-runtime specifics MEDIUM)

## Executive Summary

e2ai is a report-only autonomous QA arm built as a sibling module in the NSXBet shared-admin family. On every deploy of a registered app (bet-app first), it derives the right E2E scope from that PR's changes, runs it through a roster of specialized agents (Navigator, Vision Judge, Plan Author, Edge-case Hunter, Flakiness Analyst) that navigate by accessibility tree with screenshot verification, and publishes evidence-backed reports to an admin dashboard, a PR comment, and a neutral status check — never a gate. Research across the competitive field (Octomind, mabl, BrowserStack AI, Applitools, Meticulous, TestDino, Wopee, Paperclip) shows the table stakes are well understood (webhook triggers, AI derivation, evidence bundles, failure classification, multi-platform execution) — but nobody ships e2ai's combination: PR-diff-derived intent-level tests, a governed specialist roster, a human-promoted expected-behavior store as the verdict oracle, and a strict report-only posture. The differentiators identified are real gaps in the market, not invented ones.

The recommended approach honors house conventions and verified patterns: a Go hexagonal control plane with Postgres (River queue) and a React/RTK module registered through the standard `admin.module.json` manifest path (verified against `admin` and `incidents-admin`); Node/TS execution workers running Playwright + Stagehand on web, `simctl`/idb-primitives plus Maestro on mobile, and the Claude Agent SDK for the agent loop — all model traffic through the company aihub gateway. The architecture decomposes into three planes — control, execution, evidence — following Paperclip's proven principle that the control plane orchestrates but never runs agents, with locator-first navigation (vision as verifier, not driver), evidence-first verdicts (every verdict cites machine-checkable proof or renders `INCONCLUSIVE`), and governance (budgets, approvals, audit) from day one. Deploys are observed via GitHub `workflow_run` completion events — a shape bet-app already uses for Checkly — never PR-merge or workflow-start triggers.

The dominant risk is not technical novelty but **trustworthiness**: hallucinated passes (catastrophic on a betting platform's money paths), runaway agent spend, failure misattribution, and prompt injection through app content — each independently documented in the field. Mitigations are architectural, not prompt-based: verifier independence (separate agent, different context, ideally different model family), deterministic side-effect assertions alongside VLM judgment, a fixed verdict taxonomy, layered budget caps with a no-deploy kill switch, zero-credential egress-allowlisted agents running only against staging, and shadow-mode adoption that measures canary-bug recall before anyone relies on the verdicts. The 5-phase build order below **honors ARCHITECTURE.md's proposal** with one deliberate sharpening: the minimal governance floor (verdict taxonomy + evidence citations in Phase 1; budget caps + kill switch at agent introduction in Phase 3) moves forward out of "governance hardening" (Phase 5), per PITFALLS' phase mapping — both top-priority rows are cheap to include v1 and expensive to retrofit.

## Key Findings

### Recommended Stack

Split-runtime by domain gravity (STACK.md, confidence MEDIUM): Go for the control plane, Node/TS for everything that touches the agentic-automation ecosystem. No second datastore; one Postgres serves state and queue.

**Core technologies:**
- **Go (stable 1.25+)** — e2ai API, orchestrator, reporting — matches shared-admin family convention; session supervision, check-runs, queue consumption sit naturally here.
- **Node.js 24 LTS + TypeScript** — device-runner services: agent loop, Playwright driving, Maestro invocation — the entire agentic ecosystem is TS-first; fighting it means writing a second ecosystem.
- **PostgreSQL (company standard)** — state, results, artifacts index, and River queue backing — one database for state + queue, no Redis to operate.
- **Playwright 1.63** — web automation engine (launch/attach, screenshots, traces, video, `page.screencast`) — industry standard; never decouple Playwright version from its matching browser build.
- **Maestro CLI 2.8** — mobile driving layer for Android emulator and iOS simulator input — it maintains its own Xcode/WDA compatibility layer so the platform doesn't chase Xcode drift.
- **Claude Agent SDK (TS)** — agent runtime for derivation and QA agents — production agent loop with tools/MCP/subagents/hooks; covers "derive → run → verify → report" without a second runtime.
- **River (Go, Postgres-backed queue) + River TS client** — durable run orchestration with transactional enqueue; Node runners can enqueue follow-ups directly.
- **React + Redux Toolkit** — the module's UI inside the shared admin shell — reuse the family stack, do not fork a separate front-end.

**Critical notes:**
- **Model gateway:** the project constraint mandates the company **aihub** gateway; STACK's self-hosted LiteLLM recommendation should be read as a *feature requirements list* for aihub (per-role routing: flash-class for navigation steps, flagship vision for verdicts; per-run budget keys; spend logging — aihub already has async spend logging, verified). Do not stand up a second gateway.
- **Model choice is a routing decision, not a stack decision** — benchmark per role at kickoff and quarterly; every candidate model accepts screenshots through the gateway.
- **What NOT to use** (verified reasons): Selenium/Cypress (no trace/artifact/CDP-attach model), idb's WDA tap path on iOS 17+/Xcode 26 (open breakage — use Maestro for input, `simctl` for lifecycle/screenshots), `adb screencap` inside interactive loops (~2.1s median capture — use a scrcpy-style frame stream; screencap fine for one-off verification), Appium native↔webview switching as the primary RN strategy (chronic flakiness — vision-first sidesteps it), Asynq (needs Redis), GitHub PATs for platform identity (use a GitHub App with `checks:write` + `pull_requests:write`), self-hosted Allure/ReportPortal (the platform UI + Postgres already cover it).

### Expected Features

**Must have (table stakes):**
- Change-triggered execution via deploy webhooks + report-only PR check/comment
- AI test derivation from context (e2ai's twist: from the PR diff, not app-wide exploration)
- Vision/a11y-grounded navigation — evidence says **both, not either**: a11y tree for action grounding, vision for verifying expected behavior
- Evidence bundle per run (screenshots at step boundaries, video, traces, agent trajectory) and **deterministic verdicts from independent evidence** — agent self-report is never the verdict
- Failure classification (app bug vs test/expectation gap vs agent flake vs infra) and run history with flake rates
- Per-app config-as-code registration, secrets hygiene in artifacts, agent-facing MCP surface

**Should have (differentiators):**
- **PR-diff-derived test selection** — nobody derives new intent-level tests from the diff itself; this bounds runtime and noise and is the core bet
- **Roster of specialized agents with governed handoffs** — validated independently by Paperclip's org-chart pattern and academic rosters (arXiv 2601.02454)
- **First-class expected-behavior store with human adoption flow** — the dependency root of all differentiating value; the oracle that makes "report never fixes" coherent
- **Report-only posture** — explicitly contrasted with the field's self-healing posture; low complexity, high trust win
- Budget/cost governance per run and per agent; atomic run claiming + orphan recovery; evidence-gated flake policy with quarantine

**Defer (v2+):** mobile execution surfaces (heaviest cost — keep drivers pluggable), run-history trends/flake policy (needs accumulated runs), MCP surface, sandbox/device provisioning discipline, Meticulous-style expectation synthesis from recorded traffic (explicit anti-feature for now).

**Anti-features (explicitly NOT build):** silent self-healing of tests/locators; auto-fix PRs to app code; agent-owned merge gates; self-mutating expected behaviors without approval; pixel-perfect visual diffing as default; unbounded autonomous exploration; a general agent-orchestration framework (e2ai is a QA arm with a small fixed roster, not a platform).

### Architecture Approach

Three planes with strict separation (ARCHITECTURE.md, confidence HIGH — code-grounded): a **control plane** (Go API + Postgres: app registry, deploy state machine, diff→scope test derivation, session orchestrator, budget ledger, approval gates, audit), an **execution plane** (ephemeral workers driving surfaces through the vision loop: capture a11y+screenshot → VLM decision via aihub → act via locator/Maestro/adb → verify), and an **evidence plane** (object storage for screenshots/video/traces/journals — high-volume binaries never flow through the control plane's hot path). Every side-effecting step is a journaled, idempotent activity over an append-only run log — queue+journal durability instead of adopting Temporal. Shared-admin fit is fully verified: manifest schema, `adminModule()` Vite plugin, shell DynamicModule loading, S3 asset hosting, `publish-module` action; the BFF hosts the webhook endpoint (no webhook precedent exists in the platform — HMAC ingest must be designed fresh, GitHub's raw-body/constant-time-compare guidance applies); platform queues are domain-blind with idempotency keys.

**Major components:**
1. **Webhook receiver + app registry** — validate HMAC, idempotency-key the envelope (`repo + workflow + run_id`), 202 then enqueue; registry rows declare repo, deploy workflow, surfaces, environments, corpus root
2. **Test derivation + session orchestrator** — PR diff → affected scope → run plan; run records, budget checks, approval gates, heartbeat/wake, atomic claiming
3. **Execution workers + verdict engine** — per-surface adapters behind one closed run lifecycle (`acquire_surface → prepare_app_state → step_loop → teardown → sync_evidence`); verdicts classified from evidence + run state, never exit codes alone
4. **Evidence store + reporting** — append-only, keyed by run/session/step, URLs resolved at render time; admin module pages + sticky PR comments + neutral check runs
5. **Corpus maintainer** — post-run outcome tracking per test intent (verified-once / flaky / stale), approval-gated repair proposals

### Critical Pitfalls

1. **Hallucinated verification (HIGH)** — agents narrate plausible success, self-grade, misread screenshots; a false green on a money path is worse than no QA arm. Avoid architecturally: verifier as a separate agent with no shared trajectory (different model family for high stakes), deterministic side-effect assertions per claimed state change, evidence citations or the verdict is `INCONCLUSIVE`, adversarial canary bugs in CI weekly to measure real recall.
2. **Runaway loops and cost blowups (HIGH)** — agents can't tell "stuck" from "working"; retry storms grow context and cost; one documented run billed $4,660. Layered defense: in-loop step ceilings + per-run cost accumulators with a typed non-retryable `BudgetExhausted`, stuck detection over action history, gateway-level per-run budgets, operator kill switch without a deploy; caps sized at 3–5× measured p95 run cost.
3. **Failure misattribution (MEDIUM)** — a third of E2E failures are flakes; agent narration is unreliable for self-diagnosis; misattribution teaches devs to ignore the arm. Forced taxonomy `{APP_BUG, AGENT_ERROR, INFRA_FLAKE, TEST_DRIFT, INCONCLUSIVE}`, deterministic gates around LLM classification (confidence floors, error pages never auto-classified), infra telemetry captured alongside every run, never auto-retry `APP_BUG`.
4. **Self-healing masks real regressions (MEDIUM-HIGH)** — healing always turns red to green, which launders bugs into green checkmarks. Refuse-to-heal gate (never below confidence floor, never on error-state pages), heals only as human-reviewable diffs, system KPI = caught regressions, not healed tests.
5. **Tautological tests from diff-derived authoring (HIGH)** — tests derived from the diff by the same model family assert the implementation back at itself; bugs become expected values. Expected values from independent sources (specs, tickets, recorded prior behavior, human-approved fixtures); measure with mutation score and assertion density, never line coverage; weekly "would this fail if inverted?" spot audits.
6. **Non-determinism at temperature 0 (HIGH)** — 5–15% accuracy variation across identical runs is a hosted-inference artifact that won't go away; chained agents multiply it (0.95⁴). Structured short JSON verdicts, compare parsed fields not strings, N-run agreement for contested verdicts, verdict-flip rate as a tracked alerting metric.
7. **Prompt injection via app content (HIGH)** — page text AND pixels are attacker-adjacent input (WebInject: 0.910 success over baseline, imperceptible to humans); betting apps embed exactly the risky third-party content. Threat-model the app as untrusted: page content is data never instructions, zero credentials/egress allowlist, action allowlists, staging-only execution, full trajectory logging for forensics.
8. **Mobile platform flakiness (MEDIUM)** — iOS sim boot churn after Xcode updates, idb companion version-matching, Android emulator/Maestro animation settling, and the **WebView double-lag** (native ready ≠ web loaded ≠ JS executed — vision agents misjudge transitional frames in both directions). Pin Xcode/runner images, readiness probes not sleeps, animation/network/render-idle before capture, boot/screenshot failures auto-classified `INFRA_FLAKE`.
9. **Organizational trust collapse (MEDIUM)** — the program dies socially before technically. Shadow mode first, verified-not-self-reported as the only headline metric, evidence-rich reports with sample audits, diff-based not queue-based human review.

Plus GitHub integration facts (HIGH, official docs): webhooks are not auto-redelivered (dedup on delivery GUID, idempotent handlers, poll-based reconciliation sweep); check-run writes require a GitHub App with `checks:write`; upsert one sticky comment per PR; handle secondary rate limits.

## Implications for Roadmap

The research supports **ARCHITECTURE.md's 5-phase build order as proposed** — web happy path first, then diff-driven derivation, agent vision loop on web, mobile surfaces, corpus maintenance + governance. Every dependency discovered across the four documents confirms it: registry+ingest before agents (everything keys off it), deterministic suite before agentic navigation (proves the evidence pipeline and creates the baseline the agents must beat), web before mobile (cheapest loops; WebView-first strategy reuses the web loop inside native). The one deliberate sharpening, demanded by PITFALLS' pitfall-to-phase mapping: the two "highest" rows (verifier/verdict architecture, budgets/kill switch) land **in the first ship-worthy phases** even in minimal form — governance rides along, it does not arrive in Phase 5.

### Phase 1: Web Happy Path — Deterministic Foundation
**Rationale:** Everything keys off the app registry and deploy ingest; running the existing 49 bet-client Playwright specs proves the evidence pipeline and verdict engine with **zero LLM risk**, and creates the deterministic baseline the agentic layer must later beat. Highest-confidence territory in all research (admin manifest, BFF queue, workflow_run shape all verified in-house — mirror bet-app's real `on_deploy_checkly.yaml`).
**Delivers:** bet-app web registry row; `workflow_run` deploy ingest (HMAC validation, idempotency, 202+enqueue, poll-based reconciliation); seeded suite runs on staging; evidence-native run records (consume Playwright blob/json + traces); verdict engine with the full taxonomy (`PASS/FAIL/FLAKY/INFRA_ERROR/INCONCLUSIVE`); report page in the admin shell via `admin.module.json`; sticky PR comment + neutral check run; deploy≠live handled by rollout-health polling.
**Addresses (FEATURES):** change-triggered execution, evidence bundle (deterministic half), failure classification, per-app config-as-code, report-only posture, secrets hygiene.
**Avoids (PITFALLS):** misattribution taxonomy from day one (#3), webhook idempotency/comment-spam/permission traps, inline-webhook-processing and deploy-merge-coupling anti-patterns.
**Governance floor lands here:** verdict taxonomy + evidence citations + neutral-status semantics.

### Phase 2: Diff-Driven Derivation + Corpus Seed
**Rationale:** Cost control and relevance before autonomy — derivation bounds what runs. The expected-behavior corpus is the **dependency root of the differentiating value** (FEATURES dependency graph); seeding it from the existing specs via the learning pipeline makes the oracle real before any agent needs it.
**Delivers:** PR diff → affected-scope mapping → run plan (subset + prioritization, building on bet-client's existing `e2e:smart` concept); Plan Author agent drafting new intent-level checks, **queued for human approval** (charter-mandated); expected-behavior store first cut seeded from existing Playwright specs with the human-promote flow; dedupe against corpus.
**Addresses:** PR-diff-derived test selection (differentiator #1), NL test definitions, learning pipeline, approval queue.
**Avoids:** tautological tests (#5) — derivation expected values come from specs/tickets/recorded behavior, never from re-reading the diff's implementation; self-mutating corpus (all corpus writes approval-gated with provenance).

### Phase 3: Agent Vision Loop on Web — Verifier Independence + Budgets
**Rationale:** The agentic core proves itself on the cheapest surface. The two highest-priority pitfall rows land here **in full**: an agent roster without caps and verifier independence is one incident away from being killed — cheap now, expensive to retrofit (PITFALLS roadmap implication).
**Delivers:** session orchestrator (River queue, heartbeat/wake, append-only run log, atomic claiming, orphan recovery); Node/TS workers (Claude Agent SDK + Stagehand/Playwright); a11y-grounded locator-first navigation with screenshot verification; **separate Vision Judge agent** (independent context, ideally different model family) as the oracle against the expected-behavior store; evidence store (object storage keyed by run/session/step, no worker-local paths); verdicts cite machine-checkable evidence or render `INCONCLUSIVE`; layered budget caps (step ceiling, per-run accumulator with non-retryable `BudgetExhausted`, gateway-level keys, kill switch without deploy); stuck detection; Edge-case Hunter's bounded exploration may begin here.
**Addresses:** vision/a11y-grounded navigation, deterministic verdict with independent evidence, budget/cost governance, atomic run claiming + orphan recovery.
**Avoids:** hallucinated verification (#1), runaway loops (#2), non-determinism (#6 — structured short outputs, N-run agreement on contested verdicts), prompt injection (#7 — zero credentials, egress allowlist, action allowlist, staging-only), LLM-per-click anti-pattern.
**Hard gate before/at this phase:** confirm vision/computer-use models exist in the aihub catalog (open question #1) — request gateway support if absent.

### Phase 4: Mobile Surfaces — iOS Simulator + Android Emulator
**Rationale:** Derivation, navigation, and oracle are reused unchanged; the WebView-first strategy means much of the web loop carries into native (drive web content inside CustomWebView with the same loop; vision fallback for native chrome); EAS/Maestro wiring already exists in bet-app (`apps/native/e2e`). This is the heaviest operational surface — sequenced after the loop is proven.
**Delivers:** Android emulator via Maestro (YAML flows, native screenshots/video); iOS simulator via `simctl` lifecycle + idb primitives for grounding (accessibility info, screenshots/screencast) with **Maestro as the input/compat layer** (absorbs WDA churn — do not build on idb's WDA tap path on iOS 17+/Xcode 26); macOS runner topology; WebView double-lag handling (animation/network/render-idle before capture; DOM extraction where a debug bridge exists); boot/screenshot failures auto-classified `INFRA_FLAKE`; version-pinned runner images with deliberate upgrades behind canary runs.
**Addresses:** multi-platform execution table stake.
**Avoids:** iOS sim flakiness (#8), emulator/Maestro/WebView double-lag (#9), Appium native↔webview switching anti-pattern.

### Phase 5: Corpus Maintenance + Governance Hardening + Multi-App
**Rationale:** Autonomy grows; the system must stay trustworthy and scale to more apps. This is hardening of governance that has existed since Phases 1/3, not its introduction.
**Delivers:** Flakiness Analyst (run-history flake rates, evidence-gated retries, quarantine); stale detection + selector-repair proposals (approval-gated, refuse-to-heal gate, reviewable diffs on the PR); budget incidents/anomaly alerts; corpus-health page + trends dashboard; MCP surface for coding agents; multi-app onboarding scaffold (`adminx`-style registry rows); Edge-case Hunter formalized with bounded scheduled sweeps (opt-in).
**Addresses:** run history/trends/flake rates, flake policy with quarantine, MCP surface, multi-app registry scale-out.
**Avoids:** self-healing masking regressions (#4), trust collapse (#10 — continues shadow-mode metrics; KPI = caught regressions, canary recall measured and published).

### Phase Ordering Rationale

- **Registry + ingest before agents** — every component (derivation, orchestration, reporting, budget ledger) keys off registry rows and deploy events; verified dependency in architecture research.
- **Deterministic suite before agentic navigation** — proves evidence pipeline + verdict engine with zero LLM risk; seeds the corpus; gives the agent layer a baseline to beat. De-risks the highest-stakes pitfalls before any model is in the loop.
- **Derivation before the agent loop** — bounds runtime and cost from day one (FEATURES: "derive only what this PR could plausibly break"); the expected-behavior store must exist as oracle before the Vision Judge can be independent of it.
- **Web before mobile** — cheapest, fastest loops; WebView-first strategy reuses the web loop; mobile carries the heaviest infrastructure risk (macOS fleet, version churn) and must not block the agentic core's proof.
- **Governance rides along rather than arriving late** — the one sharpening vs ARCHITECTURE.md's phase headings: taxonomy + evidence citations in Phase 1, budget caps + kill switch mandatory at agent introduction in Phase 3, flake policy + repair governance + multi-app in Phase 5. ARCHITECTURE.md's own ordering rationale ("governance gates ride along from Phase 1") and PITFALLS' phase mapping both demand it.

### Research Flags

Phases likely needing deeper research during planning:
- **Phase 2:** derivation is the core bet with sparse direct precedent — Launchable/SAP select *existing* tests with ML; nobody derives *new intent-level tests from the diff* with an LLM. Needs prompt/schema design research, approval-queue UX, corpus data model.
- **Phase 3:** vision/CUA model availability in aihub is an open blocker (see gaps); Stagehand-vs-raw-loop choice; verifier-independence architecture details; gateway budget mechanics; determinism thresholds need empirical baselines (≥5× same-suite measurement).
- **Phase 4:** Mac runner fleet availability (open question #4) is an infrastructure prerequisite; idb/Maestro/WDA-vs-Xcode version matrix on the actual fleet; emulator snapshot strategy; WebView debug-bridge availability in bet-app.

Phases with standard patterns (skip research-phase):
- **Phase 1:** admin module manifest, BFF queue, River, Playwright execution all verified in-house. Only the webhook ingest needs a targeted design note (platform has no webhook precedent — fresh, but thoroughly documented by GitHub).
- **Phase 5:** flake detection, retention tiers, dashboards are established patterns; light research only on quarantine policy and Edge-case Hunter bounds.

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | MEDIUM | Web-researched, cross-checked across ≥2 sources, no sandbox prototyping; versions current at research date; LiteLLM recommendation conflicts with the company-gateway constraint — resolved toward aihub (see gaps) |
| Features | HIGH | Product claims grounded in official docs/READMEs read directly; complexity estimates are MEDIUM-confidence inferences |
| Architecture | HIGH | Code-grounded: admin/incidents-admin/aihub repos verified file-by-file; bet-app verified via `gh api` (workflows, playwright config, native e2e); Paperclip verified; vision-loop and durable-execution choices are MEDIUM judgment calls flagged for plan review |
| Pitfalls | MEDIUM-HIGH | HIGH on agent-reliability/cost/injection failure modes (papers read in full, independent sources agree); MEDIUM on platform flakiness; LOW items explicitly marked ([INFERENCE] staging pollution, artifact retention synthesis) |

**Overall confidence:** MEDIUM-HIGH — highest exactly where the first phases live (platform fit, ingest pattern, deterministic execution: HIGH), lowest where Phases 3–4 live (agentic runtime, mobile drivers: MEDIUM).

### Gaps to Address

- **aihub vision/CUA model catalog unverified** — blocking check for Phase 3; request gateway support if absent. Also verify per-run budget keys/spend caps on aihub (the LiteLLM virtual-key pattern maps onto it).
- **Gateway reconciliation** — STACK recommends self-hosted LiteLLM; the constraint mandates aihub. Resolution adopted here: aihub is the gateway; role-routing (flash for navigation, flagship for verdicts) and budget keys become aihub requirements. Confirm in Phase 3 planning.
- **Mac runner fleet for iOS simulators** — open infrastructure question; scope it in Phase 1 planning since topology affects cost and Phase 4 feasibility.
- **iOS input path (Maestro vs idb WDA)** — STACK (MEDIUM) and ARCHITECTURE's Phase-4 sketch (idb companion) diverge; this summary adopts Maestro-for-input/simctl-for-lifecycle per STACK's verified breakage evidence; decide finally against the actual Xcode/iOS runtime at Phase 4 planning and version-pin.
- **Verdict-flip tolerance thresholds** — set empirically in Phase 3 (≥5× same-suite measurement, alert threshold), not assumed.
- **Staging data pollution between concurrent runs** — [INFERENCE], unresolved; design per-run data namespaces or ephemeral instances in Phase 3/4; taxonomy may need a `TEST_DATA_CONFLICT` route.
- **Edge-case Hunter placement and bounds** — optional Phase 3 start, formalized Phase 5; must stay bounded (unbounded exploration is an anti-feature).
- **Checkly corpus ingestion scope** — bet-app has a real Checkly deploy hook; decide whether Checkly checks feed the corpus in Phase 2 planning.
- **Plans location per app** (app repo vs central plans repo) — registry config field; decide per-app in Phase 2.
- **Wake-on-approval scheduler internals** — targeted Paperclip follow-up only if e2ai adopts wake-on-approval semantics (Phase 3).

## Sources

### Primary (HIGH confidence)
- In-house repos, file-verified: `/Users/yuri/Workdir/Nsx/admin` (module manifest schema, `adminModule()` plugin, registry, S3, `adminx`), `/Users/yuri/Workdir/Nsx/incidents-admin` (BFF auth, domain-blind queues, no webhook precedent), `/Users/yuri/Workdir/Nsx/aihub-gateway` (hexagonal data plane, catalog, spend logging)
- `NSXBet/bet-app` via `gh api` — deploy workflow family (`on_deploy_checkly.yaml` verbatim), `playwright.config.ts` verbatim (49 specs, 3 projects incl. mobile viewports), `apps/native/e2e` Maestro configs, `e2e:smart`
- `paperclipai/paperclip` (MIT) — control-plane principle, run lifecycle, heartbeat/wake schema, approvals/budgets, audit header, workspace-relative artifacts, anti-requirements — file-by-file verified
- Official docs read directly: GitHub (workflow_run, webhook validation/redelivery, check-runs, commit statuses), Playwright (screenshots/video/trace/MCP), Maestro docs, LiteLLM docs, River docs (riverqueue.com), facebook/idb, Midscene.js, Anthropic computer use
- Papers/repos read in full: arXiv 2408.04667 (temp-0 non-determinism), arXiv 2505.11717 (WebInject pixel injection), yanazaa/selfheal (refuse-to-heal), getautonoma.com (tautological AI tests), NIST CSAI (eval cheating), agentnative.dev (cost guardrail pattern), tianpan.co (hallucinated success), pagebolt.dev (agents blind to failures)

### Secondary (MEDIUM confidence)
- Competitor/product docs: Octomind, mabl, BrowserStack AI + Test Observability, Applitools Eyes + Visual AI Guardrails, Meticulous, MagicPod, TestDino, Launchable + Facebook predictive test selection (ICSE-SEIP 2019), SAP smart-test-picker, Wopee, TestZeus Hercules, KaneAI, qa.tech
- Corroborating docs/issues: Chrome for Developers agent security, arXiv 2504.18575 (WASP), arXiv 2605.27750 (VLM grounding), arXiv 2601.02454 (multi-agent testing), AWS Well-Architected AGENTCOST07, Temporal docs, browser-use #1157/#3576, idb #853, mobile-mcp #303, Maestro #2843/#1528, patrol #2291, actions/runner-images #9591, fastlane #21411, k3s #5675, hermes-agent #17388, ITEA human-oversight paper, Microsoft triage playbook

### Tertiary (LOW confidence — validate during implementation)
- macstadium/Tracxn directory pages (version metadata only)
- Portkey/llmgateway comparisons (vendor-adjacent), developersdigest.tech LangGraph comparison
- [INFERENCE] shared-staging data pollution between concurrent runs; artifact-retention tiering synthesis

---
*Research completed: 2026-09-19*
*Ready for roadmap: yes*
