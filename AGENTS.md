# E2AI — Agent Rules

Always-on AI QA arm: report-only, agentic, vision-driven E2E testing for web + iOS Simulator + Android Emulator.

## Planning context (read before any implementation work)
- `.planning/PROJECT.md` — charter: mission, invariants, out of scope
- `.planning/REQUIREMENTS.md` — v1 scope: 45 requirements, 12 areas, phase-mapped
- `.planning/ROADMAP.md` — 5 phases with success criteria and dependencies
- `.planning/research/` — FEATURES, PITFALLS, STACK, ARCHITECTURE, SUMMARY
- `.planning/sketches/` — locked UI direction (HTML mocks; winners: 001, 002, 004, 006)

## Hard invariants
- Report-only: the platform NEVER mutates the app under test. Read + report.
- Approval gating: plans execute only after explicit admin approval.
- Budget discipline: every agent run carries time/cost/token budgets with hard stops.

## Conventions
- Commits: `gsd(<workflow>):` for planning artifacts, conventional prefixes (`feat:`, `fix:`, `docs:`, `chore:`) for code.
- GSD planning artifacts live in `.planning/`; never hand-edit config.json — use `gsd config`.
