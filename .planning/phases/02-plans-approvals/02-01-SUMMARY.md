---
plan: 02-01-PLAN.md
phase: 02-plans-approvals
status: executed
executed: 2026-09-20
---

# 02-01 Plan Summary: Plans, approvals, and test derivation

**Goal delivered:** declarative plans with content-hash identity, approval queue gating the
pipeline, PR-diff derivation with deterministic drafts, and a corpus oracle seeded from
Playwright specs — all behind the locked REST contract.

## Waves executed

- **Wave 0** — `0002_plans.sql` migration (plans, plan_versions, plan_approvals,
  corpus_behaviors, corpus_learnings), `PlansSource` on the registry App config with YAML
  parse + seed round-trip (`registry/bet-app.yaml` gains `plans_source: repo_path e2ai/plans`),
  `gobwas/glob` dependency.
- **Wave 1** — pure cores in disjoint packages: plan markdown parser (frontmatter + body),
  content-hash identity, plans sync/reconcile logic, approval state machine, corpus loader.
- **Wave 2** — `plans.PlansHTTP` REST surface and `approvals.ApprovalsHTTP` (queue + the two
  attributed decision endpoints), wire shapes locked verbatim to `web/src/lib/types.ts`.
- **Wave 3** — PR-diff derivation: plan detection from changed files (glob matchers), subset
  resolution, deterministic derive-draft job via River.
- **Wave 4** — corpus oracle: behaviors/learnings tables, promote/dismiss endpoints, seed
  from Playwright specs.
- **Wave 5 (T9 gate)** — approvals surface found unmounted in the live binary
  (`api/cmd/e2ai-api/main.go`); wired `approvals.ApprovalsHTTP` into `newMux` with the same
  degraded posture as plans (unmounted when postgres down). Verified live (see below).

## Verification evidence

- `go test ./... -count=1` — **all packages green** (app, approvals, corpus, derive, ingest,
  platform/*, report, runs, suite, verdict).
- `go vet` — clean; web `bun run build` + `validate-manifest.ts` — clean.
- **Live smoke** (scratch postgres `localhost:55432`, dev-fallback identity): API booted with
  seeded registry; `GET /api/approvals`, `GET /api/plans`, `GET /api/corpus?app=NSXBet/bet-app`
  all returned 200 JSON with the approvals queue populated from the plans repo source.
- Full-chain behavior (webhook → sync → pending → approve → approved-only execution) covered
  by the extended `api/internal/ingest` e2e test.

## Requirements satisfied

PLAN-01, PLAN-02, APPR-01, APPR-02, DERV-01, DERV-02, DERV-03, CORP-01, CORP-02 — all marked
complete in `.planning/REQUIREMENTS.md`.

## Deviations

- Approvals REST handlers existed and were tested but were never wired into the binary;
  fixed at the gate (T9) rather than left for a later wave.
