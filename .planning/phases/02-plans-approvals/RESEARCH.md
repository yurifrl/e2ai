# Phase 2: Plans, Approvals & Test Derivation — Research

**Phase:** 02-plans-approvals · **Researched:** 2026-09-20
**Confidence:** HIGH on landed-code facts (verified this session); MEDIUM-HIGH on design decisions (each closed below with resolution + fallback). No unresolved open questions.

## Summary

Phase 2 turns the Phase 1 pipeline (webhook → rollout → execute → report, verified landed) into a **human-supervised** loop: declarative markdown test plans with a content-hash identity, an approval queue gating the pipeline, PR-diff-driven plan drafting that never auto-runs, and a behavior-corpus oracle seeded from existing Playwright specs. Report-only invariant holds throughout: the platform never mutates bet-app; approvals are attributed (who, when, which content hash).

## Locked decisions (from `02-CONTEXT.md`)

- Plan format = sketch-006 contract, validated on ingest, tautology-guarded.
- Sourcing per-app via registry `plans_source` (column already reserved in `registered_apps`).
- New/changed plans queue for approval; only previously approved versions run unattended.
- Derivation drafts, never auto-runs. Corpus seeded from Playwright specs; learnings need human promotion.

## Phase Requirements

| Requirement | Summary |
|---|---|
| PLAN-01 (P2) | Declarative markdown plan format w/ frontmatter — sketch-006 is the contract |
| PLAN-02 (P2) | Plans sourced per-app: app-repo path or central plans repo, both supported |
| APPR-01 (P2) | Approval queue; only previously approved plans run unattended on deploys |
| APPR-02 (P2) | Approval UI = rendered plan + raw diff toggle (sketch-003 B); attributable (who, when, which version) |
| DERV-01 (P2) | PR diff → affected-scope mapping → subset + prioritization of the deterministic suite |
| DERV-02 (P2) | Plan Author drafts intent-level checks from the diff; drafts queue for approval before ever running |
| DERV-03 (P2) | Tautology guard: derived expected values from independent sources, never the diff's own implementation |
| CORP-01 (P2) | Expected-behavior store seeded from existing Playwright specs — first verdict oracle |
| CORP-02 (P2) | Agent observations → unverified learnings; humans promote with provenance; no self-mutation |

## Verified facts (this session, load-bearing)

1. **HTTP surface is plain `net/http` JSON REST, not Connect RPC.** `api/cmd/e2ai-api/main.go` registers `GET /api/runs`, `GET /api/runs/{id}`, `GET /api/runs/{id}/report`, `GET /artifacts/{runID}/{path...}` via `report.RunsHTTP`. The `connect RPC` mention in a doc comment is stale — no connectrpc module in `api/go.mod`. **Phase 2 endpoints follow the same plain-JSON REST pattern under `/api/`.**
2. **Identity middleware is landed:** `api/internal/platform/httpx/auth.go` — `IdentityWithConfig` reconstructs caller from BFF-injected `X-User`/`X-Permissions` headers; `UserOf` falls back to `"you"` for un-proxied dev traffic (mirrors incidents-admin `httpx.UserOf` + `DevFallbackUser`). Approval attribution reads identity from request context (`IdentityFromContext`); a malformed claim-less header yields `("", false)` → approval endpoints MUST reject unauthenticated requests (no dev-fallback approvals).
3. **Migrations:** plain embedded SQL, `//go:embed migrations/*.sql` in `api/internal/app/postgres.go`, applied in filename order, idempotent on re-boot. Phase 2 adds `0002_plans.sql` in the same style.
4. **`registered_apps` already reserves the fields:** `plans_source text, corpus_root text` with comment "future phases (columns reserved)" (`0001_init.sql:15-16`); `surfaces jsonb not null default '["web"]'`. Registry YAML (`registry/bet-app.yaml`) carries `surfaces: ["web"]`, `webhook_secret_ref`, `comment_marker`; `plans_source` TBD is the Phase 2 seed change.
5. **App struct + seeding:** `api/internal/app/service.go` — `App` mirrors DDL one-to-one; `SeedApps` upserts keyed on `(owner, repo, workflow_name, environment)`; `Bootstrap` = schema up then registry seeding. Adding `PlansSource` parsing extends `App`, no schema change to `registered_apps` needed.
6. **Pipeline gating point:** run rows are created transactionally in `ingest/dedupe.go` (delivery record + run + wait-rollout enqueue atomic). Plan resolution for a deploy happens at rollout/execute time: `suite.Execute` (SUIT-01) runs the deterministic suite; Phase 2 inserts **plan resolution before execution** — only plan versions with `status='approved'` are selected; anything else is skipped, never executed.
7. **GitHub client:** `api/internal/platform/github/client.go` has check-runs, issue comments, `ListPullsForCommit`, `ListCompletedRuns`, and a generic `do(ctx, method, endpoint, body, out)` helper with rate-limit observation. **No compare/files method yet** — Phase 2 adds `Compare()` and `ListPRFiles()` on the same `do()` pattern.
8. **Web patterns:** `web/src/lib/api.ts` (fetch client over `authenticatedFetch` + `e2aiApiBase()`, `ApiError`), `web/src/lib/usePolling.ts`, pages under `web/src/features/runs/`. Phase 2 pages mirror this under `web/src/features/plans/`, `approvals/`, `corpus/`.
9. **Run state machine:** `api/internal/runs/state.go` guarded transitions; Phase 2 does not add run states — plans carry their own lifecycle.

## Architectural Responsibility Map

| Concern | Package / file |
|---|---|
| Plan parse + validation (format contract, tautology lint) | `api/internal/plans/parse.go`, `validate.go` (new) |
| Plan sourcing + sync jobs | `api/internal/plans/source.go`, `sync.go` (new) |
| Approval state machine + REST | `api/internal/approvals/` (new) |
| Derivation (compare, scope mapping, drafting) | `api/internal/derive/` (new) |
| Corpus store + seeding + learnings | `api/internal/corpus/` (new) |
| Execution-time plan resolution gate | `api/internal/suite/execute.go` (extend) |
| REST wiring | `api/cmd/e2ai-api/main.go` `newMux` |
| UI | `web/src/features/{plans,approvals,corpus}/` (new) |

## Standard Stack

Reuse everything from Phase 1: Go + pgx + River + `goccy/go-yaml`, stdlib `crypto/sha256`, React + Vite + the existing fetch client. **One new Go dep:** `github.com/gobwas/glob` for `pr-filter` path globs (`path.Match` lacks `**`). Fallback: hand-rolled doublestar matcher (~30 LOC) if dep review rejects it.

## Architecture Patterns

### Pattern 1: Plan format contract (sketch-006, PLAN-01)

Frontmatter (scheduler reads):

| Field | Required | Rule |
|---|---|---|
| `id` | yes | Stable slug + semver suffix, `^[a-z0-9][a-z0-9-]*-v[0-9]+\.[0-9]+\.[0-9]+$`. Renaming = new plan. |
| `app` | yes | Must match a seeded `registered_apps` (owner/repo/workflow). |
| `surfaces` | yes | Non-empty subset of the app's registered `surfaces`. |
| `trigger` | yes | Enum `on-deploy` \| `on-demand` \| `nightly`. |
| `pr-filter` | no | `paths:` glob list (`apps/bet-client/checkout/**`) evaluated against the deploy PR's changed files. |
| `timeout` | no | Per-surface wall-clock budget; default 10m. |

Steps (agents read): numbered plain-language intent (no selectors — Navigator resolves UI); `judge:` required on every step (falsifiable statement — validation rejects judge lines with no falsifiable predicate: numeric bound, state assertion, or `uses:` reference); `uses:` optional corpus references — run fails if a referenced behavior is not `verified`, corpus changes flag the plan `changed`; `on-failure:` optional `continue` (default: stop surface) | `diagnose` (fresh-session evidence). Always report-only.

**Content-hash identity:** `content_hash = sha256(normalize(bytes))` where normalize = CRLF→LF, strip trailing whitespace, keep file verbatim otherwise. The hash is the version identity: every approval, run, and corpus reference pins `(plan_id, content_hash)`. Two versions with equal hashes are the same version (dedupe on sync).

**Validation rule set (ingest rejects otherwise):** required frontmatter present and enums valid; id regex; app exists + surfaces subset; ≥1 numbered step; every step has a `judge:`; `judge:` falsifiable per the heuristic above; every `uses:` key resolves to a corpus behavior with status `verified` in the DB at ingest time; unknown frontmatter keys rejected (strict).

### Pattern 2: Tautology guard, formal definition (DERV-03)

Derivation context `C = {D, K, S, B}` — the PR diff `D`, human-approved corpus behaviors `K`, spec/ticket sources `S`, recorded prior behavior `B` (past runs/verdicts). A judge statement `J(step)` is **tautological** iff `J` is entailed by the implementation of `D` alone, i.e. `J ∈ closure(impl(D))` and `J ⊄ K ∪ S ∪ B`. **Admissible iff** `J` is derivable from at least one independent source (`K`, `S`, or `B`) whose provenance ID is cited on the plan. Mechanical enforcement in Phase 2 (not the full semantic closure — that is the human approver's job):

- Plans produced by derivation MUST carry `sources:` provenance IDs; validation rejects a plan whose `sources` set is empty or entirely `{diff}`-derived.
- Lint: identifier tokens extracted from the diff must not appear inside `judge:` lines (an expected value that quotes diff code is implementation-reading, not behavior specification) — flagged, not hard-rejected, surfaced in the approval UI.
- Spot-audit metric (weekly, manual): "would this test fail if the logic were inverted?" — recorded in the approval notes. Metrics that can't be gamed: assertion density (every step has a judge) and mutation spot-checks; never line coverage.

### Pattern 3: Sourcing + sync (PLAN-02)

`plans_source` on `registered_apps` (already reserved): `{kind: repo_path, path: e2ai/plans}` (plans live in the app repo) or `{kind: central_repo, repo: NSXBet/e2ai-plans}`. Sync = list matching `.md` files at the app's head SHA via GitHub contents API, parse+validate each, and upsert by `(plan_id, content_hash)`: new hash → new `plan_version` row with status `pending_approval` if the plan has ever been approved (a change), else `draft`; unchanged hash → no-op. **Sync triggers:** (a) on boot after seeding, (b) after each successful deploy webhook for that app (fired at the end of the report stage, non-blocking, a River job `e2ai:plans:sync`), (c) manual `POST /api/apps/{id}/plans-sync`. Webhook ingest never blocks on sync. **bet-app seed change:** `plans_source: {kind: repo_path, path: e2ai/plans}` — fallback: central repo if bet-app owners prefer not to carry QA artifacts.

### Pattern 4: Approval state machine + DDL (APPR-01/02)

```
draft ──sync creates (first-ever plan)──▶ draft
draft ──human submits──────────────────▶ pending_approval
changed plan version (has approved ancestor) ─▶ pending_approval
pending_approval ──approve──▶ approved          (attributed: user, time, content_hash)
pending_approval ──reject───▶ rejected
approved ──new sync version appears──▶ superseded (old version keeps running)
```

**Invariants:** exactly one `approved` (not superseded) version per plan; the pipeline resolves only `approved` versions, so a changed plan keeps its last approved version running until a human approves the new one — "only previously approved plans run unattended" holds literally. Unapproved drafts are invisible to execution. Approval is attributable via `httpx` context identity; `DELETE`-style rejections also recorded.

DDL `api/internal/app/migrations/0002_plans.sql` (Phase 1 style: plain SQL, FKs to existing tables):

```sql
create table plans (
  id text primary key,                       -- frontmatter id, stable slug
  app_id uuid not null references registered_apps(id),
  source_path text not null,
  status text not null,                      -- draft|pending|approved|rejected (max over versions)
  latest_version_id uuid,
  updated_at timestamptz not null default now()
);
create table plan_versions (
  id uuid primary key default gen_random_uuid(),
  plan_id text not null references plans(id),
  version text not null,                     -- semver from frontmatter id
  content_hash text not null,
  content text not null,                     -- verbatim markdown
  frontmatter jsonb not null,                -- parsed + validated
  status text not null,                      -- draft|pending_approval|approved|rejected|superseded
  sources jsonb not null default '[]',       -- tautology-guard provenance IDs
  created_by text not null,
  created_at timestamptz not null default now(),
  unique (plan_id, content_hash)
);
create table plan_approvals (
  id uuid primary key default gen_random_uuid(),
  plan_version_id uuid not null references plan_versions(id),
  decision text not null check (decision in ('approved','rejected')),
  approver text not null,                    -- httpx UserOf; never 'you' in prod
  notes text,
  decided_at timestamptz not null default now(),
  unique (plan_version_id, decision)         -- one decision per version
);
create table corpus_behaviors (
  id uuid primary key default gen_random_uuid(),
  app_id uuid not null references registered_apps(id),
  key text not null,                         -- e.g. MATCH-LIVE-BADGE
  title text not null,
  description text not null,                 -- falsifiable expected behavior
  status text not null default 'unverified' check (status in ('unverified','verified','stale')),
  provenance jsonb not null,                 -- {kind: playwright-spec|run|learning, path/run_id/learning_id, commit}
  created_at timestamptz not null default now(),
  verified_by text, verified_at timestamptz,
  unique (app_id, key)
);
create table corpus_learnings (
  id uuid primary key default gen_random_uuid(),
  app_id uuid not null references registered_apps(id),
  run_id uuid references runs(id),
  observation text not null,
  status text not null default 'unverified' check (status in ('unverified','promoted','dismissed')),
  created_at timestamptz not null default now()
);
```

Seeding writes `provenance.kind='playwright-spec'` and marks behaviors `verified` (specs are human-authored — the trusted seed source); promotion/verification of anything else is human-only (APPR endpoints), so no corpus entry self-mutates. Learnings are append-only from the report stage; `promote` copies into `corpus_behaviors` with `provenance.kind='learning'` and the promoting user.

### Pattern 5: PR-diff derivation (DERV-01..03)

**GitHub compare facts (verified API semantics):** `GET /repos/{owner}/{repo}/compare/{basehead}`. Three-dot form `base...head` (merge-base compare) matches GitHub PR semantics — PR commits are not in base, so **use three-dot for deploy/PR context**; two-dot `base..head` is a direct comparison and undercounts for PRs. Payload: up to **300 files**; `files[].{filename, status: added|removed|modified|renamed|changed, additions, deletions, patch}`; `patch` omitted for binary and large (>~1MB) files; **more than 250 commits → response may truncate without warning** (compare returns at most 250 commits). When the trigger is a PR context, prefer `GET /repos/{owner}/{repo}/pulls/{n}/files` (no 300-file/250-commit truncation, richer per-file data); Phase 1 pitfall applies — `workflow_run.pull_requests` can be empty for dispatch-triggered runs, so resolve the PR via the landed `ListPullsForCommit`, with fallback: three-dot compare of head SHA against the previous successful deploy SHA stored on the last run row.

**Scope mapping model (subset, not full suite):** changed paths → (1) direct matches: plans whose `pr-filter` globs hit a changed file; (2) indirect: plans referencing corpus behaviors whose provenance paths overlap changed files; (3) else-excluded: no overlap → plan not in the subset (a deploy runs the intersection of approved plans and the mapped subset; a plan with no `pr-filter` matches on every deploy). Prioritization: direct matches first, then indirect, tie-break by most-recent failed verdict. Drafting: `e2ai:derive:draft` River job sends the (diff, corpus, mapped plans) context to the Plan Author agent, which writes intent-level plans with `sources:` provenance; drafts land as `draft` status and are surfaced in the UI for human submit → `pending_approval`. **Drafts never auto-run:** the only enqueue path to execution resolves `approved` versions exclusively, and derivation jobs have no path to the rollout/execute queues (structural, not policy, guarantee).

### Pattern 6: REST endpoints (Phase 1 style, plain JSON under /api/)

| Endpoint | Purpose |
|---|---|
| `GET /api/plans?app=&status=&limit=&offset=` | list with latest version + status |
| `GET /api/plans/{id}` | plan detail + version history |
| `GET /api/plan-versions/{versionId}/content` | verbatim markdown |
| `GET /api/plan-versions/{versionId}/diff` | unified diff vs previous version |
| `GET /api/approvals?status=pending_approval` | approval queue (badge count for sidebar) |
| `POST /api/plan-versions/{versionId}/approve` \| `/reject` | decision; identity from context; 401 unauthenticated |
| `POST /api/apps/{id}/plans-sync` | manual source sync |
| `GET /api/corpus?app=` | behaviors + learnings |
| `POST /api/corpus/learnings/{id}/promote` \| `/dismiss` | human-only corpus writes |

### Pattern 7: Approval-queue UI (sketch-003-B, APPR-02)

`web/src/features/approvals/ApprovalQueuePage.tsx` (polling via `usePolling`, status chips per `features/runs/chips.tsx` pattern) + `ApprovalDetailPage.tsx` with the locked toggle: **Rendered plan** (markdown, frontmatter rendered as the sketch's scheduler/agents reading) ↔ **Raw diff** (unified diff against the approved ancestor version), plus a decision footer (Approve / Reject + notes). Sidebar "Approvals" gets a live badge count from `GET /api/approvals`. Also: `web/src/features/plans/` (PlansList, PlanDetail with version history + which version ran on which deploy — join `runs` on `(plan_id, content_hash)`), `web/src/features/corpus/` (BehaviorsPage: coverage, stale list, promotion queue — groundwork for CORP-03). All data through `lib/api.ts` extensions (`listPlans`, `getPlan`, `getPlanContent`, `getPlanDiff`, `listApprovals`, `decideApproval`, `syncPlans`, `listCorpus`, `promoteLearning`), no new deps.

## Don't Hand-Roll

YAML parsing (goccy/go-yaml, landed), sha256 (stdlib), unified diff rendering (prettify at API side with a tiny LCS-free line-pair formatter — the sync pipeline can store the diff string; no JS diff lib needed), glob matching (gobwas/glob), GitHub calls (extend the landed `do()` client).

## Common Pitfalls (Phase-2-specific)

1. **Stale doc comment says "Connect handlers"** — the landed surface is plain net/http JSON. Build on it; do not introduce Connect.
2. **Compare API truncation** (>300 files / >250 commits) — prefer `pulls/{n}/files`; log and surface degradation in derivation output, never silently drop.
3. **`workflow_run.pull_requests` empty** (Phase 1 pitfall 1) — resolve PR via `ListPullsForCommit` on the head SHA.
4. **Race: plan changed between fetch and approval** — approvals pin `content_hash`; approving a superseded hash returns 409.
5. **Dev-fallback identity leaking into approvals** — `UserOf` returning `"you"` is a read-only convenience; approval writes MUST reject when no real identity headers are present in non-dev mode.
6. **Sync loop storms** — sync jobs enqueue at most one in-flight job per app (River unique-job / in-flight check), webhook stage enqueues without waiting.

## Open Questions — all closed

| Q | Resolution | Fallback |
|---|---|---|
| Where do bet-app plans live? | `plans_source: {kind: repo_path, path: e2ai/plans}` | central plans repo via registry change |
| REST or Connect for new endpoints? | Plain JSON REST (verified landed pattern) | none needed |
| What runs while a plan change is pending? | Last `approved` version keeps running | block the plan entirely if approvers prefer fail-closed |
| Seed corpus status from specs? | `verified` w/ provenance (human-authored specs) | seed `unverified`, batch-promote |
| Glob lib for `pr-filter`? | `github.com/gobwas/glob` | hand-rolled doublestar matcher |
| PR resolution when `pull_requests` empty? | `ListPullsForCommit` on head SHA | 3-dot compare vs previous deploy SHA |
| Approval granularity? | Per `plan_version` (content hash) | per plan id (loses change attribution) |

## Success Criteria (phase-level)

- A validated plan format with content-hash identity; invalid plans rejected at ingest with actionable errors.
- Approval queue in the admin UI (rendered/raw-diff toggle) with attributed decisions; unauthenticated approval writes impossible.
- Deploy pipeline executes only approved plan versions; changed plans keep running their last approved version.
- Derivation from a real bet-app PR yields a prioritized subset plus drafts that sit in the queue, never in execution.
- Corpus seeded from bet-app's Playwright specs with provenance; learnings captured unverified and promotable by humans only.
