# Phase 2: Plans, approvals, and test derivation - Context

**Phase Goal:** Human-supervised autonomy begins: declarative test plans with an approval queue, PR-diff-driven test selection and plan drafting, and the behavior-corpus oracle seeded from existing specs.

## Aligned Requirements
PLAN-01..02, APPR-01..02, DERV-01..03, CORP-01..02

## Specifics (auto — discuss skipped)

No user-specific ideas; decisions follow research + locked product rules (PROJECT.md):

- **Plan format (PLAN-01):** declarative markdown with frontmatter per sketch-006 contract; plan content editable by humans, validated on ingest (tautology guard: expected values must come from independent sources, never from the diff's own implementation).
- **Sourcing (PLAN-02):** per-app registry sourcing — `plans_source` field on registered apps (app repo path or central plans repo); plans discovered from source repos, content-addressed.
- **Approval queue (APPR-01..02):** new/changed plans wait for human approval; approved versions run unattended on deploys (Phase 1 pipeline consumes approved plans). Approval UI = rendered plan + raw diff of the change; approvals attributable (who, when, against which content hash).
- **Test derivation (DERV-01..03):** PR diff → affected scope → right-sized run plan (subset, not full suite). Plan Author drafts intent-level checks queued for approval — never auto-runs.
- **Corpus (CORP-01..02):** expected-behavior store seeded from existing Playwright specs; agent observations captured as unverified learnings; no corpus entry self-mutates without human promotion.

## Boundaries
- No agentic execution (Phase 3). No mobile (Phase 4). Derivation drafts, never auto-runs.
- Phase 1 pipeline (webhook → rollout → execute → report) is the only consumer of approved plans; approval state gates it.

## Deferred
None.
