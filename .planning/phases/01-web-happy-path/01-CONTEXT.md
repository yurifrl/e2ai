# Phase 1: Web happy path: deterministic foundation - Context

**Phase Goal:** The full report-only QA loop for bet-app web on every deploy: webhook → run → deterministic Playwright suite → verdicts → live HTML report + sticky PR comment.

## Aligned Requirements
INTG-01..03, APPS-01..02, INGE-01..03, SUIT-01..02, VERD-01, REPT-01..03, SECR-01, PLAT-01

## Specifics (auto — discuss skipped)

No user-specific ideas; decisions follow research recommendations:

- **Module registration:** `admin.module.json` (id `@admin/e2ai`, route `/e2ai`) registering into the shared admin shell.
- **Stack:** Go hexagonal API + Postgres + River queue (see research/STACK.md). Dev loop via Tilt, mock auth headers, no real SSO in dev.
- **Ingest:** bet-client deploy webhook; duplicate delivery replays are idempotent (dedupe key); QA gate starts only after rollout-health signal confirms build live.
- **Suite:** deterministic Playwright web suite as the first plan source; corpus seeded from INGE research; every test maps to a corpus entry (traceability).
- **Verdicts:** evidence-anchored only — every verdict carries screenshot/trace/console evidence refs; no exit-code-only verdicts (VERD-01).
- **Report:** live HTML report page (run state streams); final state + sticky PR comment on completion (REPT-01..03).
- **Secrets:** 1Password CLI / op-injected env for dev; no plaintext secrets in repo (SECR-01).

## Boundaries
- No agents/agentic loop (Phase 3). No mobile (Phase 4). No plans/approvals UI (Phase 2). No flake governance (Phase 5).
- bet-client deploy workflow + staging env are external inputs (exist today).

## Deferred
None.
