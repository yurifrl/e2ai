# Stack Research

**Domain:** Agentic, vision-driven autonomous E2E QA platform (web + iOS simulator + Android emulator), deployed on every PR/release of registered apps, reporting back into GitHub PRs
**Researched:** 2026-09-19
**Confidence:** MEDIUM (all findings web-researched and cross-checked across ≥2 independent sources where noted; official docs reads where available; no sandbox prototyping performed)

## Recommended Stack

### Core Technologies

| Technology | Version | Purpose | Why Recommended |
|------------|---------|---------|-----------------|
| Go | current stable (1.25+; verify exact minor at kickoff) | e2ai API + orchestrator + results/reporting services | Matches the shared-admin family convention (Go API, Postgres). Long-running browser/emulator session supervision, check-run and PR-comment plumbing, and durable queue consumption all sit naturally in Go. |
| Node.js + TypeScript | Node 24 LTS (verified live on this workstation: 24.16.0) | Device-runner services: the AI agent loop, Playwright driving, Maestro invocation | The entire agentic-automation ecosystem is TS-first: Playwright, Stagehand, Claude Agent SDK, mobile MCP-style tooling. Fighting this with a Go-only stack would mean writing a second ecosystem. |
| PostgreSQL | company standard (16/17) | State store, results, artifacts index, durable queue backing store | Company standard. Also backs the job queue (River), so one database serves state + queue. |
| Playwright | 1.63.0 (Sep 2026; 1.59 added `page.screencast`, 1.62 added `AbortSignal` cancellation) | Web automation engine: launch/attach, screenshot, network interception, traces, video | Industry standard for automation platforms; auto-wait, trace viewer, `connectOverCDP` for attaching to existing Chrome, self-contained HTML reports. Puppeteer lacks the trace/inspector/report depth. |
| Maestro CLI | 2.8.0 (2026) | Mobile driving layer: Android emulator + iOS simulator UI automation, including React Native | Maintains its own Xcode/WDA compatibility layer so the always-on platform doesn't chase Xcode drift itself; supports RN, Flutter, and web browsers via one declarative model; Maestro Viewer (2.6) and AI-agent debug integration (2.7) are purpose-built for agentic consumers. |
| Claude Agent SDK (@anthropic-ai/claude-agent-sdk, TypeScript) | current | The agent runtime: PR-derivation agent ("what tests does this PR need?") and role-specialized QA agents | Production-grade agent loop out of the box (same engine as Claude Code): tools, MCP tools, subagents for context isolation/parallelism, hooks, permission modes. Built for "read repo diff, decide tests, run them, report" workloads. |
| LiteLLM proxy | current, self-hosted (Docker) | Model gateway: unified OpenAI-compatible API across Anthropic/OpenAI/Gemini, routing, fallbacks, budgets, virtual keys | Vision model choice should be a per-role routing decision (fast/cheap flash-class for navigation, strong vision model for verification), not a code-level vendor lock. LiteLLM is the self-hosted standard; keys/budgets stay in-house. |
| River (Go, Postgres-backed queue) | current OSS 2026 release (Pro v0.24.0 added workflows v2, Jun 2026) | Durable job queue for run orchestration (analyze → build → execute → report), retried with transactional enqueue | Postgres-backed → no Redis to operate (unlike Asynq); transactional enqueue fits Go service; now has a TypeScript client (Jun 2026) so Node runners can insert follow-up jobs directly into the same queue. |
| React + Redux Toolkit | company standard (shared-admin) | e2ai UI surface inside shared admin: run dashboards, live status, report browser | It's a shared-admin module; reuse the family's UI stack, do not fork a separate front-end. |

### Supporting Libraries

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| @browserbasehq/stagehand | v4.0.0 (MIT, ~23.7k stars) | Natural-language browser actions (`act`/`extract`/`observe`) layered on Playwright, model-agnostic | As the default vision-driven web navigation layer for QA agents; keeps Playwright underneath for traces/screenshots. Falls back gracefully when a plain selector suffices. |
| xcrun simctl (Xcode toolchain) | bundled with macOS/Xcode | iOS simulator lifecycle (boot/shutdown/install/erase) + reliable screenshots | Always for lifecycle and capture — native, version-stable, no WDA dependency. |
| facebook/idb | current (5.3k stars, maintained) | iOS simulator programmatic primitives (screenshots, device listing, app install) | Use for non-WDA primitives on runner hosts; do NOT rely on its WDA tap path on iOS 17+/Xcode 26 (see What NOT to Use). |
| adb + uiautomator dump (Android platform-tools) | stock | Android emulator primitives: UI dump, tap/type injection, package/activities control | Base layer for Android driving; pair with a fast capture path for interactive loops. |
| scrcpy-style raw frame stream (or minicap) | current | Fast emulator screen capture (~real-time) for vision-agent navigation loops | `adb screencap` is slow (~2.1s median at 1440×3120 on an Android 15 emulator) — too slow for tight agent loops; screencap is fine for single verification shots. |
| Playwright HTML reporter | ships with Playwright | Self-contained per-run HTML artifact with embedded traces/screenshots | Attach as the raw per-run artifact; the platform's own report (Postgres + shared-admin UI + PR comment) is the aggregate view. |
| River TypeScript client | current (Jun 2026) | Insert result/follow-up jobs from Node runners into the same River/Postgres queue | Node→Go direction only; use it so runners don't need a second queue or callback HTTP hop for follow-ups. |
| gRPC (google.golang.org/grpc + @grpc/grpc-js) or plain HTTP | n/a | Go worker → Node runner handoff for session commands (Go-to-Node direction, where River TS doesn't apply) | Default to plain HTTP on localhost with a typed JSON contract; add gRPC only if streaming command traffic (e.g., screencast frames) justifies it. |

### Development Tools

| Tool | Purpose | Notes |
|------|---------|-------|
| mise | Toolchain pinning (Node 24, Go, Xcode CLIs) for runner images | Version-pin every runner image; emulator/simulator stacks are version-sensitive. |
| Docker Compose | Local e2ai stack: Postgres + LiteLLM + Go API + Node runners | Mobile simulators/emulators run on real hosts, not in Docker — compose is for services only. |
| GitHub App | Webhook ingest (`workflow_run` for deploy events), check-runs with neutral status, PR comments | Granular permissions: `checks:write`, `pull_requests:write`, `actions:read`, `contents:read`. Prefer a GitHub App over PATs. |
| macOS hosts (Mac mini fleet or self-hosted macOS runners) | Required substrate for iOS simulators | Architectural constraint, not a tool: iOS simulators cannot run on Linux. Android emulators can run on Linux runners. Plan runner topology accordingly. |

### Agent Driving Feasibility (explicit)

- **Web, vision-driven: proven pattern.** Stagehand and browser-use (60k+ stars) both ship screenshot-driven browser agents in production. Playwright's `page.screencast` (1.59+) gives a live video surface for supervising agent sessions. Feasibility: HIGH.
- **Android emulator, vision-driven: feasible with a fast capture path.** Driving taps/typing via `adb input` is reliable; the constraint is screenshot latency (see Supporting Libraries) — use a raw frame stream for the navigation loop, `screencap`/`uiautomator dump` for grounding and verification. Feasibility: HIGH with that caveat.
- **iOS simulator, vision-driven: feasible but version-sensitive.** Lifecycle + capture via `simctl` is rock-solid; input injection routes through XCUITest/WDA, which has open compatibility churn (idb issue #853 open since Oct 2023; WDA failures reported on iOS 26.2/Xcode 26.2). Maestro exists precisely to absorb this churn — pin Maestro + Xcode versions per runner image and re-verify on Xcode upgrades. Feasibility: MEDIUM-HIGH, version-pinned.
- **React Native apps (bet-app): drive the screen, not the DOM.** Appium's native↔webview context switching is a chronic pain point (recurring Appium issue patterns, e.g. #9469/#19884). Vision-first agents sidestep it; only drop to DevTools/CDP when DOM inspection is genuinely needed.
- **Model choice is a routing decision, not a stack decision.** Current frontier models (Claude/GPT-5.x-class with computer-use benchmarks, Gemini flash-class for speed) all accept screenshots through the LiteLLM gateway; benchmark per role at kickoff and per quarter after.

## Installation

```bash
# Go services (API, orchestrator, consumers)
go get github.com/riverqueue/river
go get google.golang.org/grpc   # only if streaming handoff is adopted

# Node runners (device drivers + agent loop)
npm install playwright @browserbasehq/stagehand
npm install @anthropic-ai/claude-agent-sdk
npm install @riverqueue/river.ts   # TypeScript emit client (Node→Go jobs)

# Model gateway
docker run litellm/litellm          # self-hosted LiteLLM proxy

# Mobile tooling (runner hosts)
brew tap mobile-bazel/maestro && brew install maestro
# iOS: Xcode + xcrun simctl are system-provided; idb optional:
brew install idb-companion
# Android: install Android SDK platform-tools (adb, uiautomator)

# Browser engines for Playwright
npx playwright install chromium
```

## Alternatives Considered

| Recommended | Alternative | When to Use Alternative |
|-------------|-------------|-------------------------|
| Playwright + Stagehand | browser-use (Python, CDP-direct since Aug 2025) | When a Python-only stack is acceptable and raw-CDP performance matters more than traces/reports — not our case; e2ai needs traces, artifacts, and TS ecosystem coherence. |
| Maestro | Appium 2.x (+ XCUITest/UIAutomator2 drivers) | When deep native-object queries or existing Appium suites must be reused. Heavier setup, same WDA churn underneath; keep as an escape hatch, not the default. |
| River (Postgres) | Asynq (Redis) | Only if the platform already operates Redis and no Postgres-transactional enqueue semantics are needed. Adds a second datastore here. |
| Claude Agent SDK | LangGraph | When orchestrating graph-shaped multi-model pipelines with checkpoint/rewind semantics as the core product. e2ai's loop is "derive → run → verify → report"; the SDK covers it with less runtime weight and no Python dependency. |
| LiteLLM | Portkey (TS-first, hosted control plane, guardrails/cache) | If guardrail-driven policy or hosted caching is a hard requirement and a vendor control plane is acceptable. Self-host + in-house secrets favor LiteLLM. |
| Playwright HTML report + platform UI | Allure Report | Only for teams that already live in Allure ecosystem; its Java toolchain and annotation overhead add moving parts the platform UI + Postgres already cover. |
| Claude Agent SDK (TS) | Raw Anthropic SDK hand-rolled loop | Only for minimal single-tool experiments; you'd rebuild subagents, permission modes, and hooks badly. |

## What NOT to Use

| Avoid | Why | Use Instead |
|-------|-----|-------------|
| Selenium / WebDriver legacy stack | No trace/artifact model, slower protocol, no CDP attach; the agentic ecosystem has moved past it | Playwright |
| idb WDA tap/injection path on iOS 17+ / Xcode 26 | Open compatibility breakage (idb #853; WDA fails on iOS 26.2/Xcode 26.2 per mobile-mcp #303) | Maestro for input; `simctl` for lifecycle/screenshots |
| adb screencap in the interactive agent loop | ~2.1s median per capture at 1440×3120 — kills loop responsiveness | scrcpy-style raw frame stream; screencap for one-off verification shots |
| Appium native↔webview context switching as primary RN strategy | Chronic, flaky (appium #9469/#19884-class issues); adds chromedriver coupling | Vision-first screen driving; CDP only when DOM access is required |
| Cypress | No multi-tab, no CDP attach to running Chrome, weaker network/trace tooling for agents | Playwright |
| Asynq for the job queue | Requires Redis alongside Postgres — second datastore for no benefit here | River |
| GitHub PAT for platform identity | Repo-scoped PATs can't manage check-runs cleanly; no fine-grained app permissions | GitHub App with `checks:write` + `pull_requests:write` |
| Self-hosted ReportPortal/Allure server | Overkill: platform's own Postgres + shared-admin UI + PR comments cover aggregation, trends, and delivery | Playwright HTML artifact + platform UI |

## Stack Patterns by Variant

**If a QA agent is stuck on a surface (vision fails, state unclear):**
- Use a layered ground truth: screenshot → accessibility/DOM snapshot → `uiautomator`/XCUITest element dump
- Because pure-vision agents plateau on dense UIs; hybrid grounding is the field's pragmatic consensus (Stagehand's `observe` is built for this)

**If model spend spikes:**
- Use LiteLLM role-routing: flash-class models for navigation steps, flagship vision models for verdicts, with budget keys per app/run
- Because navigation is high-frequency/low-stakes; verification is low-frequency/high-stakes — routing (not prompting) is where cost is controlled

**If an Xcode/Android SDK upgrade breaks mobile driving:**
- Use version-pinned runner images + Maestro as the compatibility layer; only re-pin after a canary run passes
- Because WDA-vs-Xcode churn is the #1 operational risk for always-on iOS automation (verified: idb #853, mobile-mcp #303)

**If a run must outlive a deploy (long emulator session, flaky network):**
- Use River durable jobs with retries + run-state in Postgres, never in-memory goroutine state
- Because the platform is always-on and unattended; crash-recovery semantics are a product requirement

**If Node and Go must exchange frequent, low-latency commands (frame streams, live control):**
- Use plain HTTP/JSON for the command path and River TS for the enqueue path; introduce gRPC only for the streaming path
- Because Go→Node has no River direction (River TS is Node→Go only), and HTTP-on-localhost is boring and debuggable

## Version Compatibility

| Package A | Compatible With | Notes |
|-----------|-----------------|-------|
| Playwright 1.63 | Chromium build shipped by `npx playwright install` | Never decouple: pin Playwright version in runner image and install its matching browsers. |
| Maestro CLI 2.8 | Xcode/Android SDK versions on the runner | Maestro absorbs WDA churn internally, but re-verify after every Xcode upgrade; pin `maestro --version` per image. |
| idb / WDA | iOS ≤ 16.5 mostly; iOS 17+ / Xcode 26 has open failures | Lifecycle/capture OK on modern iOS via `simctl`; do not build on idb's WDA tap path. |
| River Go ↔ River TS client | Same River/Postgres schema generation | TS client (Jun 2026) inserts jobs the Go workers consume; keep versions aligned on upgrades. |
| Claude Agent SDK | Node 24, MCP tool servers | SDK is TS/Node-native; no Python runtime needed. |
| LiteLLM proxy | Upstream provider API shapes (vision/image blocks) | LiteLLM normalizes OpenAI-compatible schemas; re-check provider changelogs when adding a new vision model. |

## Sources

- Playwright GitHub releases (v1.59.0 screencast, v1.62.0 AbortSignal; traceary.com catalog for 1.62/1.63) — MEDIUM, cross-checked
- browser-use "why we moved from Playwright to CDP" migration post (Aug 2025) + browser-use repo — MEDIUM, cross-checked
- Stagehand: npmjs.com/package/@browserbasehq/stagehand + browserbase docs (v4.0.0, MIT, Playwright-based) — MEDIUM, cross-checked
- facebook/idb README + issue #853 (iOS 17 support, open) — MEDIUM, cross-checked with mobile-mcp issue #303 (WDA on iOS 26.2/Xcode 26.2)
- Maestro docs: docs.maestro.dev (CLI 2.6 Viewer, 2.7 AI agent debug, 2.8 current; web browsers support; known issues) — MEDIUM (official docs)
- Appium WebView context-switching issue patterns (appium #9469, #19884) — MEDIUM, cross-checked
- adb screencap latency analysis (handsets blog, May 2026: ~2.12s median @1440×3120) — MEDIUM, single detailed source
- LiteLLM docs (router architecture, virtual keys, fallbacks; docs.litellm.ai verified 2026-09-09) — MEDIUM (official docs)
- Portkey vs LiteLLM comparisons (portkey.ai, llmgateway.io, Apr 2026) — LOW-MEDIUM (vendor-adjacent, cross-checked)
- Claude Agent SDK docs (code.claude.com: subagents, tools, hooks; anthropics/claude-agent-sdk-typescript) — MEDIUM (official docs)
- LangGraph overview + Claude Agent SDK vs LangGraph comparison (developersdigest.tech, Jun 2026) — LOW-MEDIUM
- River: riverqueue.com (docs, blog; River for TypeScript Jun 2026; Pro v0.24.0 workflows v2 Jun 2026; pkg.go.dev) — MEDIUM (official docs)
- GitHub webhooks/check-runs docs (workflow_run event, check-runs create/update with neutral status, issues API comments) — MEDIUM (official docs)
- Playwright HTML reporter + Allure comparison (allurereport.org/docs/playwright) — MEDIUM
- macstadium/Tracxn directory pages for version metadata — LOW (directory data only)

---
*Stack research for: e2ai — agentic, vision-driven autonomous E2E QA arm (@admin/e2ai), NSXBet shared-admin family*
*Researched: 2026-09-19*
