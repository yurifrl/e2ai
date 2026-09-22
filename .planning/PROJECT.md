# E2AI — the Always-On AI QA Arm

## What This Is

`@admin/e2ai` — an autonomous QA arm and sibling module in the NSXBet shared admin family. On every deploy of a registered app (first: `NSXBet/bet-app`, any app by config), it derives the right E2E tests from that PR's changes, runs them through a roster of specialized AI agents with vision-driven navigation across web, iOS simulator, and Android emulator, and reports — never blocks — via live HTML reports with screenshots and explanations. It continuously curates a database of expected behavior, turning every deploy into verified product knowledge.

## Core Value

An arm of testers always reporting live, never blocking: every deploy produces rich, explained, evidence-backed QA reports while growing a trustworthy database of expected behavior.

## Requirements

### Validated

<!-- Shipped and confirmed valuable. -->

(None yet — ship to validate)

### Active

<!-- Current scope. Building toward these. These are hypotheses until shipped and validated. -->

- [ ] Deploy watch: GitHub webhook catches deploys of registered apps (bet-app first) and triggers the arm
- [ ] Smart test selection: derive relevant test scope from the PR's changes (diff-aware)
- [ ] Declarative markdown test plans, sourced per-app via config (app repo or central plans repo)
- [ ] Plan approval queue: new/changed plans wait for human approval; previously approved plans run automatically
- [ ] Agent roster of specialized agents: Navigator (driver), Vision Judge (verifier), Plan Author (planner), Edge-case Hunter (free explorer), Flakiness Analyst (stability)
- [ ] Vision-driven navigation as the core verification mechanism (perceive like a user, resilient to markup churn)
- [ ] First-class surfaces: web app + iOS simulator + Android emulator (WebView-aware)
- [ ] Expected-behavior database: agent observations auto-captured as unverified learning; humans promote to verified baselines
- [ ] Learning pipeline: ingest existing Playwright specs (and Checkly usage if present) into plans/baselines
- [ ] Live HTML reports with screenshots and plain-language explanations (Paperclip-inspired presentation)
- [ ] Report surfaces: admin dashboard + PR comment + neutral GitHub status check — never a failing gate
- [ ] Multi-app registry: apps on-boarded by config; bet-app is the first wired entry
- [ ] Native sibling module: `admin.module.json` (id `@admin/e2ai`, route `/e2ai`), React+RTK web, hexagonal Go API, registers into the shared admin shell
- [ ] All model traffic routed through the company AI gateway

### Out of Scope

<!-- Explicit boundaries. Includes reasoning to prevent re-adding. -->

- Blocking merges/deploys on QA failure — the arm is report-only by design; gatekeeping stays with existing CI
- Slack notifications — chosen surfaces are dashboard, PR comment, and status check; Slack adds a second report pipeline without a consumer
- Paperclip as runtime substrate — decided: adopt its patterns (org chart, approvals, budgets, audit, skills), build native in the house stack; its CLI-heartbeat agent model mismatches sub-second vision loops and its Node stack/auth/Postgres would collide with admin-shell integration
- Auto-running unapproved plans — new/changed plans must be human-approved first; only previously approved plans run unattended

## Context

**Company landscape.** The shared admin platform (`NSXBet/admin`) hosts modules; each module is its own repo declaring `admin.module.json` (id, routeBase, navigation, commands, permissions, owners) against `@nsxbet/admin-sdk`/`admin-ui`. Reference sibling: `incidents-admin` — React + Vite + Tailwind web with RTK feature slices, hexagonal Go API with layered domains, Connect-RPC + BFF proxy, Postgres-backed `platform/jobs` queue, otel, Tilt/K8s namespace, mock-auth local stack.

**Target app.** `NSXBet/bet-app` is a Turborepo: `bet-client` (Next.js web), `native` (React Native/Expo, WebView-first), `sso` (Keycloak), `rules`; shared packages incl. `@nsxbet/ui` (Brasa/Tamagui) and `@nsxbet/test-utils`. Existing Playwright E2E lives in `apps/bet-client`, CI-sharded via `NSXBet/test-orchestrator`. Deploys flow through changesets → Version Packages PR → tags → manually dispatched deploy workflows — so the webhook must key on deploy-workflow completion events, not pushes.

**Paperclip (pattern donor).** `paperclipai/paperclip` (MIT) is an agent-orchestration control plane: companies of agents, org charts, goals→tasks, heartbeats, approval gates, budgets, audit, skills store. We take its presentation and governance patterns (live visual reports, approval queues, per-agent cost/audit) and none of its runtime.

**Mobile reality.** The native app is WebView-first; emulator agents must drive and judge webview content. Drivers under research: Playwright (web), iOS Simulator via idb/screencast, Android Emulator via adb/Maestro.

**Model substrate.** Company AI gateway — no direct provider keys; agent model choices flow through it.

## Current Milestone: v1.1 Admin Cockpit

**Goal:** Move every control-plane action of the QA arm (triggering apps, webhook handling, job operations) out of ad-hoc API/script calls and into the authenticated admin UI.

**Target features:**
- Cockpit: admin section to trigger apps/plans directly (manual trigger, today webhook|reconcile|sweep sources only)
- Admin action surface: every outside action (webhook simulate, agent enqueue, MCP probe, GitHub redelivery) becomes an admin UI action
- Webhook inbox: admin view of incoming webhook deliveries with retry
- Sync jobs: observe and control the River sync/periodic job system

## Constraints

- **Tech stack**: Shared admin conventions — React + RTK feature slices (web), hexagonal Go domains (api), Postgres, Connect-RPC, Tilt/K8s — the module must register in the shared admin shell like its siblings.
- **Governance**: New/changed plans require human approval; only approved plans execute unattended on deploys.
- **Non-blocking**: The arm never gates merges, deploys, or CI; it reports only.
- **Model access**: All LLM traffic via the company AI gateway.
- **Platform scope (v1 surfaces)**: Web + iOS Simulator + Android Emulator; physical device farms deferred.
- **Security**: bet-app is a private production system; reports and agent transcripts stay inside the authenticated admin.

## Key Decisions

<!-- Decisions that constrain future work. Add throughout project lifecycle. -->

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| Paperclip: inspired-by, not adopted | QA domain logic is the product; generic orchestration overlaps in-house primitives (`platform/jobs`, admin governance); CLI-heartbeat agents mismatch sub-second vision loops; Node/second-auth would fight admin-shell integration | — Pending |
| Own sibling module repo (`@admin/e2ai`) rather than a bolt-on inside an existing module | Shared-admin family pattern (like `incidents-admin`); isolates the QA domain; clean `admin.module.json` registration | — Pending |
| Multi-app by config from day one, bet-app first | User wants any-given-app configurability, not a bet-app-only tool | — Pending |
| Agent roster: Navigator, Vision Judge, Plan Author, Edge-case Hunter, Flakiness Analyst | Specialized agents outperform one generalist; roles map to distinct reports | — Pending |
| Expected-behavior DB: auto-capture unverified, human-promote to verified | Grows knowledge continuously without trusting agent guesses | — Pending |
| Deploy trigger: GitHub webhook (deploy-workflow completion) | bet-app deploys are manual `workflow_dispatch` runs; webhook catches their completion | — Pending |
| Vision-driven navigation as core verification | Judges what a user sees; resilient to markup/classname churn | — Pending |
| Models via company AI gateway | Company convention; no provider keys in agents | — Pending |

## Evolution

**After each phase transition** (via `/gsd:transition`):
1. Requirements invalidated? → Move to Out of Scope with reason
2. Requirements validated? → Move to Validated with phase reference
3. New requirements emerged? → Add to Active
4. Decisions to log? → Add to Key Decisions
5. "What This Is" still accurate? → Update if drifted

**After each milestone** (via `/gsd:complete-milestone`):
1. Full review of all sections
2. Core Value check — still the right priority?
3. Audit Out of Scope — reasons still valid?
4. Update Context with current state

---
*Last updated: 2026-09-19 after project initialization*
