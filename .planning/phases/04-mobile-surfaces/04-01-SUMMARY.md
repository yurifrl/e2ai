---
plan: 04-01-PLAN.md
phase: 04-mobile-surfaces
status: executed
executed: 2026-09-20
---

# 04-01 Plan Summary: Mobile surfaces (Android + iOS)

**Goal delivered:** the agentic loop reaches real devices — SurfaceDriver seam extended to
Android (Maestro-first) and iOS (simctl lifecycle + Maestro input layer), readiness probes
with INFRA_FLAKE classification, per-run runner-image pinning with a canary job, and both
mobile drivers proven against a real emulator and simulator.

## Waves executed

- **Wave A (T1, T2)** — `d1a5fe8`: reconciled the broken Go↔TS env contract for agent
  spawn (worker.go env naming ↔ loop.ts fence cutover; the debt flagged by the T7 sandbox
  sibling in phase 3). `3980a91`: mobile tooling bootstrap doc — maestro 2.10.0 (install
  script; the brew `maestro` cask is an unrelated product), Android SDK cmdline-tools +
  platform-tools 37 + emulator 37.1.11 + API 35 google_apis arm64 image, idb-companion 1.6.1
  (facebook/fb tap), openjdk. AVD `e2ai-pixel` created by T5 (T2 scope interpreted as
  install-only — folded into the consumer).
- **Wave B (T3, T4)** — `5752eec1`: SurfaceDriver seam (`surface.ts`), web driver
  extraction, android/ios scaffolds; `35a4096`: surface propagation in Go —
  `runs.surface` column (migration 0005), plan frontmatter validation against the locked
  vocabulary, `E2AI_SURFACE` spawn env.
- **Parallel (T7, T8)** — `68e4ddf`: readiness probes (`readiness.ts` ProbeRunner with
  injectable signals) + INFRA_FLAKE failure-class vocabulary mirrored to Go
  `verdict/classify.go`. `62dfd20`: per-run runner-image pinning (runs-level image
  selection + version persistence), canary river job registered in the api binary.
- **Device smoke wave (T5, T6)** — Android `fe3b8364` + iOS `1b1ecf8c`: both drivers
  validated on real devices end-to-end (boot → navigate → observe → act → capture →
  shutdown), with wedge recovery, `app://` native launch (Android), `--udid` pinning (both
  CLIs auto-attach otherwise), and clean skips when tooling is absent.

## Verification evidence

- `go vet ./...` clean; `go test ./...` — **all packages green** (agents, app, approvals,
  corpus, derive, ingest, plans, platform/*, report, runs, suite, verdict).
- agentrunner: `tsc --noEmit` clean; `npm test` — **89/89 pass** with the Android emulator
  and iOS simulator smokes running concurrently (~84s).
- **Android smoke (AVD e2ai-pixel, pixel_7, API 35 arm64):** cold-boot with
  `adb wait-for-device` + `sys.boot_completed` readiness (420s deadline), `app://` launch of
  Settings via resolve-activity + `am start`, real uiautomator hierarchy in `observe()`,
  real `maestro test` flow for `act()`, PNG screenshot verified by magic bytes, wedge
  recovery proven against a SIGSTOP-frozen emulator, teardown verified (`adb devices` empty).
- **iOS smoke (iPhone 17 sim, iOS 26.5):** boot → `status_bar override` → openurl → Safari
  → `listapps` observe → PNG capture → shutdown; maestro act pinned by `--udid` (2.10
  otherwise attaches to a running Android emulator).
- Real devices caught three bug classes the unit tests (mock-validated) missed:
  inverted simctl arg orders (`status_bar`/`openurl`/`io`), maestro 2.10's mandatory flow
  config section, and CLI device auto-attachment — all fixed with device-run evidence
  (`a586ae89`, `814c895d`, `2ed6ce1b`).

## Requirements satisfied

PLAT-01 (phase 1 delivery, checkbox lag fixed here), PLAT-02, PLAT-03, PLAT-04, PLAT-05 —
marked complete in `.planning/REQUIREMENTS.md`.

## Deviations

- T2 did not create the AVD (read its scope as install-only); T5 created `e2ai-pixel` where
  it is consumed.
- T3's scaffolds were validated against mocks only; real-device smokes corrected the
  scaffolds in smoke-owned commits rather than reopening T3.
- Disk was 100% full at the emulator boot step; resolved with an authorized
  `brew cleanup -s --prune=all` (~6.5 GB, re-fetchable cache only). API-35 AVDs hard-clamp
  userdata to 6 GB and refuse to boot below ~7.4 GB free at the AVD dir.
