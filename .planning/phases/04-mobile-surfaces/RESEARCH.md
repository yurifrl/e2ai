# Phase 4 Research: Mobile surfaces

**Compiled:** 2026-09-20 · Sources: `.planning/research/` (STACK, PITFALLS, ARCHITECTURE, FEATURES, SUMMARY), live codebase scan of `agentrunner/` + `api/internal/` (T4-integration map), local Mac environment probe.

## 1. Locked decisions (carried from prior research — do not re-litigate)

| Decision | Choice | Citation |
|---|---|---|
| Android driving | Maestro CLI (YAML flows) + adb base layer | STACK.md Core Technologies |
| iOS lifecycle | `xcrun simctl` (boot/shutdown/install/erase/capture) | STACK.md Supporting Libraries |
| iOS input | Maestro (its Xcode/WDA compat layer absorbs Xcode drift) | SUMMARY.md Gaps resolution |
| iOS grounding | idb primitives only (a11y info, screenshots); **never idb WDA tap** on iOS 17+/Xcode 26 (idb #853, mobile-mcp #303) | STACK.md What NOT to Use |
| WebView | Drive the screen, not the DOM; readiness = animation-settle + network-idle + render-idle before capture; CDP/DevTools only when DOM access genuinely needed | STACK.md Agent Driving Feasibility; PITFALLS.md #9 |
| Flakes | Boot/screenshot/adb-disconnect failures → `INFRA_FLAKE` with boot log attached; retries only for infra class; never auto-retry APP_BUG | PITFALLS.md #3, #8 |
| Runner discipline | Version-pinned Xcode/emulator/Maestro per image; upgrades only behind passing canary run | STACK.md Stack Patterns; SUMMARY.md Phase 4 |
| Scarcity | Simulators/emulators are the scarce resource — **serialize use**; 1–2 apps = one worker, sims on a single Mac | ARCHITECTURE.md Scalability |
| Substrate | macOS host for iOS sims (never Linux/Docker); Android emulator may be Linux but local-first is Mac | STACK.md Development Tools |

## 2. Environment probe (this Mac, 2026-09-20)

- Xcode + `xcodebuild` present; CoreSimulator runtimes iOS 26.4 / 26.5; devices incl. iPhone 17 Pro/Air/17e, iPad Pro M5 — all Shutdown.
- **Missing:** `maestro` CLI, `adb`, Android SDK (`~/Library/Android/sdk` absent), `idb-companion`.
- Brew gotchas verified locally: brew `maestro` cask is an unrelated product (runmaestro.ai 0.17.3) — **install mobile.dev Maestro via official script** (`curl -Ls https://get.maestro.mobile.dev | bash`, installs to `~/.maestro/bin`); `idb-companion` is **not in homebrew-core** — requires `brew tap facebook/fb` first.
- Tooling bootstrap is therefore an explicit execution task with `--version` acceptance, not an assumption.

## 3. Codebase integration map (live scan)

**Headline: the surface abstraction is a seam, not an implementation.** T1–T4 + T7 landed (sessions/worker/loop skeleton/sandbox); T5 Navigator + T6 vision judge are NOT landed (`agentrunner/src/` has no navigator.ts; stagehand dep declared, unused; `index.ts` prints "no role entry wired yet"). Mobile extends the seam — never a parallel system.

| Extension point | Verdict | Evidence |
|---|---|---|
| `StepExecutor` / `StepVerifier` injection | CLEAN | `agentrunner/src/loop.ts:126-141,154-155` |
| `capture()` + `LoopDeps.page: Page` | REFACTOR — Playwright `Page` leaks; a native/webview surface has no Page | `loop.ts:79-99,145` |
| Plan frontmatter `surfaces` + subset gate | CLEAN — new surface strings validate as-is | `api/internal/plans/parse.go:36,192-195`, `validate.go:45-57` |
| Surface → execution propagation | MISSING — nothing tells the loop which surface it drives (no env, no session column, no driver selection) | scan |
| Worker ↔ agentrunner env contract | **BROKEN** — Go sets `E2AI_API_BASE`, `E2AI_SERVICE_TOKEN`, `AIHUB_BASE`, `AIHUB_TOKEN_REF`, `E2AI_ROLE_MODEL`, never `E2AI_RUN_ID`; TS requires `E2AI_API_BASE_URL` (`api.ts:45`), `E2AI_RUNNER_SERVICE_TOKEN` (`api.ts:46`), `AIHUB_BASE_URL`+`AIHUB_API_TOKEN` (`gateway.ts:86-87`), `E2AI_AGENT_MODEL_<ROLE>` (`gateway.ts:103-108`), `E2AI_RUN_ID` (`index.ts:87,90`). A spawned navigator boot-fails today. `AIHUB_TOKEN_REF` has no consumer. | `worker.go:208-216` vs `api.ts/gateway.ts/index.ts` |
| Evidence storage/serving (`session_steps.evidence`, `artifacts`, `ServeArtifact`) | CLEAN reuse — mobile screenshots/videos ride same kinds | `0004_agents.sql:44`, `report/artifacts.go:14-60` |
| Budget engine / sandbox / fences | CLEAN, surface-agnostic; note `loop.ts:37 fenceBlock` is an admitted placeholder duplicate of hardened `fences.ts` — reconcile while touching the loop | `budget.go:50-133`, `sandbox.ts`, `fences.ts:42-53` |
| Runner image pinning / canary | PARTIAL — per-run `ContainerSpec.Image` seam clean (`execute.go:95`); `suite.Executor.Image` env-fixed (`main.go:64`); report `Version` parsed (`parse.go:90-92`) but not persisted (no `runner_image`/version column) → canary recall metrics have nowhere to live | `execute.go`, `main.go`, `0001_init.sql:35-53` |
| Suite pipeline (`entrypoint.sh`: clone/build/serve/playwright) | REFACTOR boundary — web-only; mobile runner needs its own pipeline story, keep `ContainerRunner` as the seam | `runner/Dockerfile`, `execute.go:113-160` |
| Playwright pin | DRIFT — agentrunner `playwright ^1.49.0` (`package.json:20`) vs suite image pin `1.58.2` (`runner/Dockerfile:9`); align before mobile work | scan |

Sandbox navigation check is URL-origin-only (`sandbox.ts:121-123`); mobile needs a deeplink/screen-allowlist story.

## 4. Known pitfalls + mitigations (mobile)

- **Pitfall 8 — iOS sim on CI chronically flaky:** boot fails after Xcode updates; screenshot timing catches half-drawn frames. Mitigations: pin versions, explicit boot-wait readiness probes (never sleep), warm-boot reuse, retry ONLY boot/screenshot class, auto-classify INFRA_FLAKE, keep idb companion matched to Xcode/iOS matrix. (PITFALLS.md #8)
- **Pitfall 9 — Android emulator + Maestro timing + WebView double-lag:** boot/KVM issues, adb disconnects mid-run, `waitForAnimationToEnd` timeout bugs (maestro #2843/#1528); native chrome ready ≠ web loaded ≠ JS executed — VLM "verifies" transitional UIs. Mitigations: wait-for-idle before every capture, network-idle + render-idle signals, hardware accel, snapshot reuse, prefer DOM/state extraction when a debug bridge exists, screenshot timing = named flake category. (PITFALLS.md #9)
- **Capture speed:** `adb screencap` ~2.1s median (1440×3120) — too slow for tight agent loops; Maestro native capture / fast frame stream for the loop, screencap for one-offs. (STACK.md)
- **VLM judging limits:** downsampled screenshots destroy small text; crop/zoom region under judgment; send only claim-relevant frames. (PITFALLS.md #1)
- **Artifact growth:** video only on failure/retry at scale; tiered retention. (PITFALLS.md Moderate)

## 5. Open questions → how this plan resolves them

1. **Mac CI fleet** — deferred (out of scope v1: local-Mac-first per charter; PLAT-05 covers image discipline, not fleet).
2. **iOS input final call** — resolved: Maestro input / simctl lifecycle / idb grounding-optional, never WDA on iOS 26 (research divergence already settled in SUMMARY.md; this plan adopts it).
3. **Version matrix on fleet** — resolved empirically at tooling-bootstrap time on this Mac; pinned in docs + canary check.
4. **Emulator snapshot strategy** — optional mitigation; document, don't build (MVP scope).
5. **WebView debug bridge in bet-app** — probe during readiness work; fallback = render-idle via consecutive-stable-frame check + network-idle via Maestro/adb signals.
6. **Staging data pollution across concurrent runs** — pre-existing; not mobile-specific; stays out of Phase 4.
