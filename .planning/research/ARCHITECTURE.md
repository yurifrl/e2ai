# Architecture Research

**Domain:** Autonomous, vision-driven AI QA control plane (event-driven agent orchestration over web + iOS/Android surfaces)
**Researched:** 2026-09-19
**Confidence:** HIGH — grounded in primary sources: in-house repos (`/Users/yuri/Workdir/Nsx/admin`, `incidents-admin`, `aihub-gateway`; `NSXBet/bet-app` via `gh`), the Paperclip OSS repo (paperclipai/paperclip, verified file-by-file), and official docs (GitHub Actions/webhooks, Playwright, Meta idb, Maestro, Temporal, River, Anthropic computer use). Web-only claims are tagged; code-grounded claims are HIGH.

## Standard Architecture

### System Overview

The right decomposition is **control plane / execution plane / evidence plane**, a separation Paperclip runs on successfully (verified principle: "The control plane doesn't run agents. It orchestrates them. Agents run wherever they run and phone home." — `docs/start/architecture.md`). e2ai adds a third plane: evidence (screenshots, traces, videos) is high-volume binary data that must never flow through the control plane's hot path.

```
┌────────────────────────────────────────────────────────────────────────────┐
│ EVENT INGEST (shared admin BFF)                                            │
│  GitHub webhooks (workflow_run, HMAC-SHA256 validated) · CI dispatch       │
│  POST /webhooks/github → validate sig → 202 → enqueue (idempotency key)    │
└──────────────┬─────────────────────────────────────────────────────────────┘
               │ deploy/merge events (envelope + raw payload)
┌──────────────▼────────────────────────────────────────────────────────────┐
│ CONTROL PLANE (Go API + Postgres, module-scoped)                          │
│  App registry · deploy state machine · job queue (River-like)             │
│  Test derivation (diff → scope → test intent) · session orchestrator      │
│  Budget/credit ledger · approval gates · audit log                        │
└──────────────┬───────────────────────────────┬────────────────────────────┘
               │ run spec (queue message)       │ run status (append-only)
┌──────────────▼───────────────┐   ┌───────────▼──────────────────────────┐
│ EXECUTION PLANE (workers)    │   │ EVIDENCE PLANE                        │
│  Agent roster per surface:   │   │  S3/blob: screenshots, videos,        │
│   · web → Playwright         │──▶│  traces, DOM/a11y snapshots, step     │
│   · iOS sim → idb (companion)│   │  journals — referenced by key/URL,    │
│   · Android emu → Maestro    │   │  never inlined                        │
│  Vision loop: capture →      │   └───────────┬──────────────────────────┘
│  VLM decide → act → verify   │               │ evidence refs
│  (LLM via aihub gateway)     │               │
└──────────────┬───────────────┘   ┌───────────▼──────────────────────────┐
               │ run record        │ REPORTING (admin module UI)           │
               └──────────────────▶│  Run timeline · per-test verdicts w/  │
                                   │  visual evidence · PR comments ·      │
                                   │  corpus health (flaky, stale, gaps)   │
                                   └───────────────────────────────────────┘
```

### Component Boundaries

| Component | Responsibility | Communicates With |
|-----------|---------------|-------------------|
| **Webhook receiver** (BFF) | Validate GitHub HMAC signature, normalize `workflow_run` payload, reject replays, 202-ack then enqueue | Event queue, app registry |
| **App registry** | Per-app config: repo, deploy workflow name, tag scheme, environments, surface list, target URLs, corpus root | Ingest, orchestrator, reporting |
| **Deploy state machine** | Transition deploy events → `building → deployed → rolled out → verified/failed` (deploy ≠ live: ArgoCD rollout lag) | Ingest, orchestrator |
| **Test derivation** | Map PR diff → scope → test intent (affected routes/features → corpus subset) → run plan | Registry, orchestrator |
| **Session orchestrator** | Queue of run specs; assigns one agent per session (Paperclip: single-assignee atomic checkout); budget checks; approval gates; wake/retry machinery | Queue, execution plane, LLM gateway |
| **Execution workers** | Drive surfaces: Playwright (web), idb (iOS sim), Maestro (Android emu); run the screenshot→decide→act→verify vision loop | Control plane, evidence plane, LLM gateway |
| **Evidence store** | Object storage + metadata: screenshot per step, video, trace, a11y/DOM snapshot, step journal; append-only | Execution plane, reporting |
| **Verdict engine** | Classify pass/fail/flaky/infra-error from evidence + run state, not exit codes alone | Orchestrator, reporting |
| **Reporting** | Admin module pages + PR comments: run timeline, verdicts with visual evidence, corpus health | Control plane, evidence store |
| **Corpus maintainer** | Post-run: dedupe test intents, flag stale/flaky specs, propose repairs (approval-gated) | Registry, orchestrator |

### Data Flow (trigger → report, explicit)

1. **Trigger.** GitHub delivers `workflow_run` (action `completed`) for a registered deploy workflow — e.g. bet-app's real hook: `workflows: ["Release - Web"]`, filtered by `conclusion == 'success'` and `display_title` containing `Deploy to production` (verbatim from `on_deploy_checkly.yaml`, verified via `gh api`).
2. **Ingest.** Receiver validates `X-Hub-Signature-256` (HMAC-SHA256, constant-time compare), builds an idempotency key (`repo + workflow + run_id`), stores the envelope, enqueues, and 202s immediately (best practice per GitHub docs; no inline processing).
3. **Registry resolution.** Job resolves app config from registry (slug, surfaces, env URLs, corpus root). Unknown app → quarantine event for admin triage, not a crash.
4. **Derivation.** If PR context available, derive scope from diff (routes/components touched → candidate test intents). Else run seeded suite. Dedupe against corpus.
5. **Dispatch.** Orchestrator creates a run record, checks budget policy (like Paperclip's `budget_policies` → `budget_incidents`), creates per-surface sessions, enqueues execution jobs (River-style Postgres queue, transactional enqueue).
6. **Execution.** Worker spins the target surface (browser context / simulator boot via idb companion / emulator + Maestro), then loops: capture state (screenshot + a11y tree) → LLM decision via aihub gateway → act (Playwright locator/idb HID tap/Maestro step) → verify. Each loop appends to a step journal and uploads evidence by key.
7. **Verdict.** From evidence + run state: pass / fail (with reproducing evidence) / flaky (pass on retry, like bet-client's existing `retries: 2`) / infra-error (simulator boot failure ≠ product bug).
8. **Report.** Admin module renders run timeline + verdicts with inline visual evidence; PR comment summarizes with evidence links. Failures above a threshold go to an approval/attention flow rather than auto-mutating anything.
9. **Feedback.** Corpus maintainer records outcome per test intent (verified-once / flaky / stale), feeding the next derivation pass.

### Shared-Admin Module Fit (verified against `/Users/yuri/Workdir/Nsx/admin` and `incidents-admin`)

- **Registration:** e2ai ships `web/admin.module.json` validated against `packages/admin-sdk/schemas/admin.module.schema.json`; the `adminModule()` Vite plugin emits `dist/module.manifest.json`; the shell's `DynamicModule` fetches the manifest and loads the hashed entry at runtime; the registry records it (`POST /api/modules` / `GET /api/catalog`); assets host on S3 (`assets-admin.webdevelopers.com.br`); publish via the existing `publish-module` GitHub action; scaffold with `adminx init`. No bespoke plumbing — this is the exact path incidents-admin uses.
- **Ingress:** the BFF is the natural webhook endpoint host. **No webhook precedent exists in the platform** (incident.io sync is deliberately poll-based, `api/internal/integration/incidentio_jobs.go:18-22`) — HMAC validation must be designed fresh; GitHub's own guidance applies (raw-body capture before parse, proxy must not modify payload, constant-time compare).
- **Processing model:** platform API queues are **domain-blind** with idempotency keys — enqueue a `deploy-event` job and let the e2ai service consume it. API does JWT-free trust of BFF identity headers (`api/internal/platform/httpx/auth.go` reads `X-User`); service-to-service uses Keycloak service tokens.
- **LLM access:** route all model calls through **aihub-gateway** (Go hexagonal, OpenAI/Anthropic-compatible, model catalog + routes in Postgres, async spend logging). Verify computer-use/vision models are in the catalog — if not, that's a gateway feature request, and budget/spend tracking comes free.
- **UI:** navigation sections + permission-gated commands per manifest schema; report pages live inside the shell like incidents-admin's pages.

## Patterns to Follow

### Pattern 1: Control plane not execution plane
**What:** Orchestration state (runs, sessions, verdicts, budgets) lives in Postgres-backed services; agents/workers are ephemeral and stateless, holding only a run context (Paperclip pattern: env `PAPERCLIP_API_URL/API_KEY/COMPANY_ID/TASK_ID/RUN_ID`, every mutation audited via run-id header).
**When:** Always for e2ai — QA runs die with their machines; state must not.
**Why:** Resumability after worker death, auditability of every agent action, cost attribution per run.

### Pattern 2: Privilege-separated webhook ingest via `workflow_run`
**What:** Observe deploys with a `workflow_run`-triggered workflow (or webhook), never a PR-hook pipeline. GitHub verifies: `workflow_run` actions are `requested|in_progress|completed`; it fires only when the workflow file is on the default branch; and the triggered run receives secrets/write tokens even when the triggering run didn't — the official privilege-separation pattern. Untrusted PR code is never checked out by the privileged workflow (the "pwn requests" warning). Bet-app already uses this shape for Checkly: `workflow_run: {workflows: ["Release - Web"], types: [completed]}` with a conclusion + display-title filter.
**When:** The e2ai trigger path.
**Why:** Runs on deploy completion regardless of who dispatched it (in-house deploys are manual `workflow_dispatch` → reusable `NSXBet/github-workflows/deploy_parallel.yaml@main`, keyed on `bet-client@*` tags — a deploy hook must observe *that* event chain, not PR merges).
**Caveats to encode:** (a) deploy ≠ live — ArgoCD rollout lag (bet-app sleeps 300s; e2ai should instead poll rollout health); (b) the default-branch requirement for the observing workflow; (c) `workflow_run` chains cap at 3 levels.

### Pattern 3: Adapter/executor abstraction with a closed run lifecycle
**What:** One interface, N surface adapters (web/iOS/Android). Paperclip's verified lifecycle: closed allowlist of phases (`place_workspace → start_transport → create_runtime → ensure_session → prepare_turn → turn → end_session → … → release`) with per-phase timings logged; adapters parse runtime output into structured results (usage, cost, artifacts). e2ai analog: `acquire_surface → prepare_app_state → step_loop → teardown → sync_evidence`.
**When:** All execution code.
**Why:** Adding a surface must not touch the orchestrator; phased run logs make every failure diagnosable.

### Pattern 4: Vision loop over accessibility trees, vision as verifier
**What:** Don't ask the model to click from pixels blind: capture **a11y tree + screenshot**, prefer structured locators (Playwright locators / idb accessibility-info / Maestro's accessibility-tree driving — verified) and use the VLM to decide *what to do next*, judge *did the UI respond correctly*, and repair broken selectors. Anthropic's computer-use reference loop (screenshot → coordinate action → screenshot) is the fallback for genuinely visual-only checks (e.g., ad banners, canvas, WebView chrome).
**When:** Web: locator-first with screenshot verification. Native WebView screens: same loop (the web content inside CustomWebView is reachable; pure-native chrome needs vision). Visual-only assertions: screenshot diff/VLM judgment.
**Why:** Cost and determinism. Pure screenshot-click loops are slow, expensive, and flaky; locator-first keeps agent steps auditable.

### Pattern 5: Evidence-first verdicts (never trust exit codes alone)
**What:** Every step appends a journal entry with evidence keys; verdicts cite evidence. Playwright facts (official docs): screenshots can be buffer/element/fullPage; **video is only produced by `recordVideo` and finalized at context close** (so teardown must close contexts before evidence sync); traces are per-test (`trace: on-first-retry` in bet-client's config); browser contexts isolate state. idb provides per-command screenshots and screencast; Maestro provides screenshots/video natively.
**When:** Verdict engine + reporting.
**Why:** "Failed" without a picture is unactionable; "pass" without evidence is unauditable. bet-client already emits blob/json reporters — e2ai can consume those formats for the seeded suite and go evidence-native for agent sessions.

### Pattern 6: Heartbeat/wake + append-only run log with idempotent retries
**What:** Long agent sessions need a coordination primitive that survives worker death. Paperclip's verified machinery: `heartbeat_runs` + `heartbeat_run_events` (append-only, per-run `seq` cursor, writer locks the run row, rejects sequence gaps, idempotent retry), `agent_wakeup_requests` for wake-on-approval flows.
**When:** Session orchestrator.
**Why:** A 10-minute simulator session that dies at minute 8 must resume or fail cleanly, not corrupt state; approvals (e.g., "publish this failing report") wake the agent with structured context.

### Pattern 7: Governance by default — budgets, approvals, audit
**What:** Copy Paperclip's verified trio: (1) budget policies with threshold → incident (costs tracked per agent/task/project — for e2ai, per app/run); (2) approval gates as decision-ready payload cards with structured wake (a failing-run report that would post to a customer channel requires sign-off); (3) every mutation audited with a run-id header. Tool-gateway-style policies (`allowed / ask_first / off`) fit the corpus-maintainer's repair actions: suggesting a selector fix = allowed; rewriting app code = blocked.
**When:** From day one, not retrofitted.
**Why:** An autonomous system that spends credits and posts publicly without gates is the top organizational risk (see PITFALLS).

### Pattern 8: Durable execution for long sessions
**What:** Queue+worker (River-style Postgres, transactional enqueue, per-queue `MaxWorkers`) is sufficient for run-level dispatch; per-step durability inside a session comes from the append-only run log, not a workflow engine. Temporal's model (workflow code re-executed on replay; activities' results recorded in event history) is the gold standard but heavy to adopt; its one killer idea — **record, then replay** — is achievable by making every side-effecting step an idempotent, journaled activity.
**When:** Session orchestrator design.
**Why:** Don't buy a second runtime to do what a journal + idempotent steps do.

## Anti-Patterns to Avoid

### Anti-Pattern 1: LLM-per-click everything
**What:** Every navigation action routed through an expensive vision model.
**Why bad:** 49 existing bet-client specs × dozens of steps × vision calls = cost blowout and minutes-long sessions; Paperclip costs budgeting exists precisely because of this.
**Instead:** Locator-first execution with VLM at decision points and verification; reserve pure-vision loops for visual-only assertions.

### Anti-Pattern 2: Deploy-merge coupling
**What:** Triggering QA on PR merge or on deploy *workflow start*.
**Why bad:** In-house deploys are manual dispatches of a reusable workflow keyed on tags (`bet-client@*`); the QA-relevant moment is *rollout completion*, not tag push. Merge-triggered runs test code that may never deploy.
**Instead:** Observe the `workflow_run` completion of the registered deploy workflow + rollout health polling (bet-app's Checkly hook sleeps 300s — poll instead).

### Anti-Pattern 3: Inline webhook processing
**What:** Doing derivation/simulation inside the HTTP handler.
**Why bad:** GitHub times out deliveries at 10s; a slow handler causes redelivery storms; replays without idempotency duplicate runs.
**Instead:** Validate → persist envelope with idempotency key → enqueue → 202. Process async.

### Anti-Pattern 4: Absolute paths / host coupling in artifacts
**What:** Evidence records pointing at worker-local paths (`/tmp/runner-3/shot.png`).
**Why bad:** Paperclip explicitly forbids it — work products are `workspace_file` refs relative to the workspace only (verified `doc/AGENT-ARTIFACTS.md`); host paths break evidence replay on any other machine.
**Instead:** Evidence keyed by run/session/step in object storage; metadata in Postgres; URLs resolved at render time.

### Anti-Pattern 5: Silent self-healing of the product
**What:** Agent patches app code when a test fails, or "fixes" by retrying until green.
**Why bad:** Paperclip's verified anti-requirement: "Not automatically self-healing — surfaces problems, doesn't silently fix them." A QA arm that mutates product code destroys trust in both directions.
**Instead:** Surface via attention/report; corpus repair (selector updates) is approval-gated and only touches e2ai's own corpus.

### Anti-Pattern 6: One global test corpus with no app scoping
**What:** Single shared suite across apps/environments.
**Why bad:** bet-app is not one surface: `bet-client` (Next.js, 49 specs, 3 browser projects incl. Pixel 5/iPhone 12 viewports), `native` (expo-router + CustomWebView + Maestro flows at `apps/native/e2e/`), `sso`. Registry-less selection guarantees wrong-surface runs and meaningless verdicts.
**Instead:** App registry rows declare surfaces, corpus roots, env URLs; derivation is registry-driven.

### Anti-Pattern 7: Trusting agent verdicts without replayable evidence
**What:** Storing only "FAIL: could not find checkout button".
**Why bad:** Unauditable; LLMs mis-see screenshots; teams will demand repros.
**Instead:** Journal + screenshots + video + trace; verdict references evidence; seeded-suite runs additionally emit Playwright blob/json (bet-client's existing reporters) for parity with current E2E.

## Scalability Considerations

| Concern | 1–2 apps, weekly deploys | 5–10 apps, daily deploys | 20+ apps, per-PR |
|---------|--------------------------|--------------------------|------------------|
| Ingest | Single BFF webhook endpoint + one queue | Partitioned queues per app; dedupe window (deploy storms) | Fan-out registry; per-repo webhook apps (org-wide GitHub App w/ Actions permission beats per-repo secrets) |
| Execution | One worker; simulators on a single Mac (idb companion model) | Worker pool per surface; mobile runners are the scarce resource — serialize simulator use | Remote device farm / ephemeral emulators; web runners autoscale |
| Evidence | Single S3 bucket, manual TTL | Lifecycle rules (30d hot, archive older); video only on failure/retry | Tiered storage; trace/video sampling |
| LLM calls | Direct via aihub gateway | Rate-limit per app; budget incidents at thresholds | Token budgets per run type; derive-first (locator) policies enforced by orchestrator |
| Reporting | One run timeline page | Per-app dashboards; corpus health page | Roll-ups, trend analysis, flakiness SLOs |

## Recommended Build Order (each phase delivers value alone)

1. **Phase 1 — Web happy path (deterministic first).** Registry row for bet-client web + `workflow_run`-based deploy observation (mirror `on_deploy_checkly.yaml`) → run the *existing* 49 Playwright specs on staging → evidence-native run records (consume blob/json + trace) → verdict engine (pass/fail/flaky/infra) → report page in admin shell via the standard module manifest. **Value:** deploy verification that exists today, no agents.
2. **Phase 2 — Diff-driven derivation.** PR diff → affected-scope mapping → run plan (subset + prioritization); dedupe against corpus; `e2e:smart` already exists in bet-client — build on that concept rather than replacing it. **Value:** right-sized runs, cost control from day one.
3. **Phase 3 — Agent vision loop on web.** Orchestrator + step journal + evidence store + LLM-via-gateway; agent navigates/verifies locator-first with screenshot verification; agent-authored checks for gaps derivation finds. **Value:** coverage where specs are missing; the agentic core proves itself on the cheapest surface.
4. **Phase 4 — Mobile surfaces.** iOS simulator via idb (companion+client; accessibility-info for locators, screenshots/screencast) and Android emulator via Maestro (YAML flows, native screenshots/video; native app already uses Maestro at `apps/native/e2e/`). WebView-first strategy: drive web content inside CustomWebView with the same web loop; vision fallback for native chrome. **Value:** full-surface QA; the EAS/Maestro wiring already exists in bet-app.
5. **Phase 5 — Corpus maintenance + governance hardening.** Flaky/stale detection, selector repair proposals (approval-gated), budget incidents, multi-app onboarding (`adminx init`-style scaffold for new registry rows). **Value:** the system stays trustworthy as autonomy grows.

**Ordering rationale:** registry+ingest before agents (everything keys off it); deterministic suite before agentic navigation (baseline verdicts + evidence pipeline proven); web before mobile (fastest feedback, cheapest loops); derivation before scale (cost); governance gates ride along from Phase 1 (report publishing) rather than arriving late.

## Confidence Assessment

| Area | Confidence | Basis |
|------|------------|-------|
| Admin platform fit (manifest, BFF, queue, auth) | HIGH | Verified in `/Users/yuri/Workdir/Nsx/admin` + `incidents-admin` source |
| Deploy ingest pattern (`workflow_run`) | HIGH | GitHub official docs (MEDIUM per seam) **cross-checked** against in-house `on_deploy_checkly.yaml` (verbatim, verified) → verified pair |
| Bet-app deploy/test reality | HIGH | `gh api` reads of `NSXBet/bet-app` (workflows, playwright.config.ts verbatim, tree) |
| Paperclip architecture patterns | HIGH | File-by-file verification of paperclipai/paperclip (docs, schema, services, routes) by repo scout |
| Vision loop (computer use) | MEDIUM | Anthropic official docs + Meta/Microsoft corroborating docs; no code-grounded instance yet |
| Durable execution choice (queue+journal over Temporal) | MEDIUM | Temporal + River official docs; judgment call, flagged for plan review |
| aihub gateway as LLM path (vision models availability) | MEDIUM | Gateway repo verified (routing/catalog/spend), but vision/CUA model catalog unverified — open question |

## Open Questions

1. **Vision/computer-use models in aihub gateway catalog** — if absent, request gateway support (Anthropic computer-use / equivalent) before Phase 3.
2. **ACP transport stability** — Paperclip pins ACP packages via overrides (`codex-acp@1.6.2`, `claude-agent-acp@0.73.0`), indicating churn; e2ai likely speaks plain tool-JSON to its own agents instead, but if we reuse agent runtimes, transport stability needs a targeted check.
3. **Scheduler policy internals** (Paperclip heartbeat cadence/backoff) were not deep-read; if e2ai adopts wake-on-approval semantics, do a targeted follow-up.
4. **Simulator fleet**: whether Mac runners exist for CI-side iOS simulator execution at NSX (idb companion needs a macOS host) — infrastructure question for the plan phase.

## Sources

**Verified (HIGH) — local/source-read:**
- `/Users/yuri/Workdir/Nsx/admin` — module manifest schema, `adminModule()` Vite plugin, registry API, S3 hosting, `adminx` CLI, publish-module action (AdminPlatformScout, file-verified)
- `/Users/yuri/Workdir/Nsx/incidents-admin` — BFF auth (`httpx/auth.go`), domain-blind queue + idempotency, poll-based incident.io sync (no webhook precedent)
- `/Users/yuri/Workdir/Nsx/aihub-gateway` — Go hexagonal data plane, OpenAI/Anthropic-compatible endpoints, model catalog routes, async spend logging
- `NSXBet/bet-app` via `gh api` — `on_deploy_checkly.yaml` + deploy-family workflows (verbatim), `playwright.config.ts` (verbatim, 3 projects incl. mobile viewports), 49 specs, `apps/native/e2e` Maestro configs, `e2e:smart`, build-args per env, k8s manifests (BetAppScout + direct fetches)
- paperclipai/paperclip (MIT, 81k★) — control-plane principle, adapter registry + run lifecycle (`doc/run-log-events.md`), heartbeat/wake schema, approvals/budgets/costs (`doc/SPEC.md` §6), `X-Paperclip-Run-Id` audit header, workspace-relative artifact refs (`doc/AGENT-ARTIFACTS.md`), Smoke Lab browser runner (gated, puppeteer+chromium), anti-requirements §12 (PaperclipScout, file-verified)
- facebook/idb — companion+client architecture, HID/accessibility/screenshot/video commands, MIT (GitHub repo read)

**Official docs (MEDIUM, cross-checked where noted):**
- GitHub: Webhook events and payloads (`workflow_run`), Events that trigger workflows (default-branch requirement, pwn requests, 3-level chain cap), Validating webhook deliveries (HMAC-SHA256), Best practices for webhooks
- Playwright: screenshots, videos (context-close finalization), trace
- Maestro: how-it-works (accessibility-tree driving), flows, `?ask=` doc API
- Anthropic: Computer use tool (screenshot→action loop, toolsets); corroborated by Meta/Microsoft computer-use docs (web search, MEDIUM)
- Temporal: workflows/durable execution (replay + activity event history); River: Postgres queues, transactional enqueue, per-queue workers
- mabl blog: AI agent frameworks for E2E testing (landscape corroboration)
