# Stack Research: v1.1 Admin Cockpit

**Project:** E2AI (`@admin/e2ai`) — milestone v1.1 Admin Cockpit
**Researched:** 2026-09-22
**Confidence:** HIGH — every finding below was verified against this repo's actual code (file paths cited); River v0.47.0 APIs verified against the vendored module source at `~/go/pkg/mod/github.com/riverqueue/river@v0.47.0/`. No web-sourced claims.

## The One Sentence

**v1.1 requires zero new dependencies.** The cockpit is a new web feature-section plus a new Go surface package, both built strictly from patterns that already exist in this repo; the only genuinely new artifacts are one schema migration and a few new registry columns/fields.

## Reality Check Against the Milestone Constraint

PROJECT.md's constraint says "React + RTK feature slices (web), Connect-RPC (api)" — the shared-admin *family* conventions. **This module demonstrably does neither**, and v1.1 must follow module reality:

- **No RTK.** `web/package.json` has no redux/rtk dependency and `web/src/` has no `@reduxjs` import. Data access is a hand-rolled fetch client (`web/src/lib/api.ts`) + a polling hook (`web/src/lib/usePolling.ts`) + per-page `useState` mutation handling (`web/src/features/plans/ApprovalsQueuePage.tsx:24-28` — `busyId`/`actionError` pattern). Introducing RTK in v1.1 would put a second data paradigm in one module. If the family later mandates RTK, that is a module-wide migration, not a cockpit item.
- **No Connect-RPC.** `api/go.mod` has no connectrpc dependency. Every surface is Go 1.22+ `net/http.ServeMux` method-patterns (`"GET /api/runs"`, `api/cmd/e2ai-api/main.go:344-347`) with DTO structs locked verbatim to `web/src/lib/types.ts` (`api/internal/report/page.go:1-4`, `api/internal/plans/http.go:1-5`). The wire contract is the TypeScript interfaces; keep that.
- **Decision:** propose logging a PROJECT.md decision amendment — "e2ai follows the admin family's *integration* conventions (manifest registration, BFF routing, identity headers) but uses net/http + fetch/polling for data, per its locked v1.0 contract." The roadmapper should not write RTK/Connect phases.

## Recommended Stack

### Web — Cockpit Section (new feature area in `web/src/`)

| Technology | Version (pinned in repo) | Purpose | Why / Evidence |
|------------|---------|---------|----------------|
| `@nsxbet/admin-sdk` | ^0.14.0 (`web/package.json`) | Shell registration, `useFetch`, `useI18n`, `env`, telemetry | Already the module's integration point. Exports verified in `web/node_modules/@nsxbet/admin-sdk/dist/index.d.ts`: `AdminShell`, `useFetch`, `useI18n`, `env.adminGatewayUrl`, `track`. Cockpit nav = new entry in `web/admin.module.json` `navigation.sections` + a `commands[]` item with `route: "/e2ai/cockpit"`, mirroring the 7 existing commands (`web/admin.module.json:44-170`). |
| react-router-dom | ^6.28.0 | Cockpit routes | Add routes to `web/src/app/routes.tsx` (plain `RouteObject[]`, `useRoutes` in `App.tsx`). Links use absolute routeBase (`/e2ai/...`) per `RunsListPage.tsx:52`. |
| Existing fetch client | `web/src/lib/api.ts` | All cockpit API calls | Extend `getJson`/`postJson` helpers with the new cockpit endpoints. Zero new libraries; the contract stays "api.ts ↔ types.ts ↔ Go DTOs". |
| `usePolling` | `web/src/lib/usePolling.ts` | Live-ish inbox/job views | Already visibility-aware ("avoid hammering a busy API") — exactly right for report-only cockpit dashboards. 10s list / 5s detail cadence matches `RunsListPage.tsx:2-3`. |
| Tailwind + existing primitives | 3.4.17 | Layout/chips/badges | `web/src/features/plans/chips.tsx` and `StatusChip` in runs are the established look. `@nsxbet/admin-ui` is imported **only** for `styles.css` (`web/src/index.css:2`) — do not start importing its components mid-milestone without checking they exist in v0.6.0. |
| i18n locales | `web/src/i18n/locales/{en-US,pt-BR,es,ro}.json` | All cockpit strings | Manifest is 4-locale (`web/admin.module.json:6-26`); every page uses `useI18n`. Cockpit ships all four keys on day one. |
| Mutation UX | `ApprovalsQueuePage.tsx:24-28,51-57` | Action buttons | Per-action `busyId` + `actionError` + `await postJson(...)` then local refresh. Cockpit actions (trigger, kill, retry, pause) copy this verbatim. Destructive actions (kill, queue pause, GitHub redelivery): two-step confirm button pattern (arm → execute) in-component — no modal dependency. |

**Explicitly rejected for web:** RTK/RTK Query (see reality check); SSE/WebSocket live push (polling already covers the need; new infra violates budget discipline); any modal/toast library (two-step buttons + inline error banner are the house pattern).

### API — Cockpit Surface (new `api/internal/cockpit/` package)

| Technology | Version (pinned in repo) | Purpose | Why / Evidence |
|------------|---------|---------|----------------|
| `net/http` ServeMux surface | Go 1.26.6 (`api/go.mod`) | All new endpoints | New package mirrors `flakes.QuarantineHTTP` / `trends.TrendsHTTP` shape exactly: `Mount(mux)` with method-pattern routes, `gate(w,r,perm)` helper (`api/internal/plans/http.go:111-123`), `writeErr` JSON error shape. Mounted in `main.go` `newMux` behind a `nil`-guard like siblings (`main.go:336-376`). |
| `httpx` identity + permission ladder | `api/internal/platform/httpx/auth.go:35-37` | Auth wall + attribution | `admin.e2ai.view` (reads), `admin.e2ai.edit` (triggers/enqueues), **`admin.e2ai.admin` — already defined, never used; v1.1 is its first consumer** (kill, queue pause/resume, redelivery, job cancel). Actor attribution via `httpx.IdentityFromContext(r.Context()).User`, same as approvals (`api/internal/approvals/http.go:271,287`). Every cockpit write records the actor. |
| River v0.47.0 client methods | `github.com/riverqueue/river v0.47.0` | "Sync jobs" observe + control | Verified in module source `client.go`: `JobList(ctx, *JobListParams)` with `NewJobListParams().First(N).States(...)` + cursor pagination; `JobCancel`, `JobRetry`, `JobDelete`, `JobGet`; `QueueList`, `QueueGet`, `QueuePause`, `QueueResume`. Thin read-only-ish Go handlers over these — **no new dependency**. Scope note: cockpit controls *job instances and queues*; the periodic-job *schedule* stays in code (`WithPeriodicJobs`, `api/cmd/e2ai-api/main.go`). |
| pgx/v5 queries | 5.11.0 | Inbox reads | `webhook_events` and `webhook_triage` already store everything the inbox needs: verbatim `payload jsonb`, `delivery_id`, `signature_valid`, `occurrences`/`last_seen_at` (`api/internal/app/migrations/0001_init.sql:24-33`, `0007_hardening.sql:34-48`). **No triage HTTP surface exists today** — `ingest.ListTriage` (`api/internal/ingest/triage.go:83-90`) has no mount in `main.go`; the inbox fills this real gap. |
| Ingest re-drive for retry | `api/internal/ingest/dedupe.go`, `http.go` | Webhook retry (primary path) | The whole ingest path is transactional and idempotent by construction (`dedupe.go:1-4`, replay safety in `suite/execute.go:548-551`). Retry = re-parse the stored `webhook_events.payload` and re-drive the same `Ingest(ctx, seed, queue)` seam with a `retry` provenance — converges instead of duplicating. Prefer this over GitHub-native redelivery (below) as the primary action because it needs no new credentials surface and works even when the hook is gone. |
| `platform/github` client (extended) | `api/internal/platform/github/client.go` | GitHub-native redelivery (secondary action) | Hand-rolled REST client with rate-limit tracking (`client.go:46-76`) — add `POST /repos/{owner}/{repo}/hooks/{hook_id}/deliveries/{delivery_id}/attempts` the same way (no `go-github` introduction). **Requires `github_hook_id` on `registered_apps`** (new registry field in `registry/*.yaml` + column; `betweb.yaml.example`/`bet-app.yaml` gain the field; secret stays `webhook_secret_ref op://` per SECR-01). |
| MCP probe | `api/internal/mcp/http.go` | "MCP probe" admin action | `POST /mcp` JSON-RPC already exists and is read-only. Cockpit probe = post a `tools/call` envelope server-side (or via the surface) and render the response. No new dependency. |
| Agent enqueue / kill | `api/internal/agents/http.go:40-56` | "Agent enqueue" action | Already implemented and permission-gated (`POST /api/runs/{runId}/agent-sessions`, kill endpoint present). The cockpit feature is UI + a queue-data view over `GET /api/agent-sessions` — mostly wiring, not new plumbing. |
| Budget visibility | `api/internal/trends/http.go:33` (`GET /api/trends/cost/{runId}`), `trends.go:81-101` (`RunCost.cost_millicents`) | Cost panel beside triggers | Triggers are billable (agent sessions cost money). Cockpit trigger dialog shows the target's recent per-run cost so the human sees the price before firing — cheap reuse, honors the budget-discipline invariant. |

### Manual Trigger Design (the one genuinely new pipeline path)

Today's trigger vocabulary is `webhook | reconcile | sweep` (`api/internal/ingest/dedupe.go:25-27`, enforced by a check constraint widened in `0007_hardening.sql`).

| Concern | Recommendation | Evidence/rationale |
|---------|---------------|-------------------|
| New source value | `manual` — migration `0010_cockpit.sql` widens the check constraint | Pattern set by 0007 widening the same vocabulary. |
| `workflow_run_id` for manual runs | Synthesize from a dedicated Postgres sequence starting at 10^12 (GitHub run IDs currently ~10^9; decades of headroom) | `runs` has `unique (app_id, workflow_run_id)` (`0001_init.sql:52`); a manual trigger has no GitHub run. A reserved ID band avoids touching the uniqueness invariant that dedupe depends on. Alternative rejected: relaxing to a partial unique index — it weakens the webhook dedupe guarantee the whole pipeline leans on. |
| Execution semantics | Manual triggers for **plans** enqueue a draft through the existing approval queue (reuse the approvals machine, `api/internal/approvals/machine.go`, and the `POST /api/runs/{id}/derive-draft` precedent at `main.go:405`); manual triggers never bypass approval gating | PROJECT.md governance invariant: "New/changed plans require human approval". |
| Webhook-simulate action | Compose a synthetic `RunSeed` server-side from a chosen registered app + environment and drive the same `Ingest` seam with `TriggerManual` | The seed path is already the single transactional funnel (`dedupe.go`); simulating anywhere else would fork the pipeline. |

### Infrastructure

| Technology | Purpose | Why / Evidence |
|------------|---------|----------------|
| Tilt/K8s (unchanged) | Dev loop | `Tiltfile` already forwards api :8082, postgres :5434, admin-bff :8080, web :3005; BFF routes `/e2ai` prefix → e2ai-api (`config/admin-bff.yaml:30-38`). Cockpit adds **no** infra: same BFF route, same namespace. |
| admin-bff identity headers | Auth in dev | `web/vite.config.ts` proxy injects `X-User`/`X-Permissions: admin.e2ai.view` — dev testing of edit/admin-gated cockpit actions needs the proxy header list extended with `admin.e2ai.edit,admin.e2ai.admin` (one-line change in `web/vite.config.ts`). |
| Migrations runner | `app.Bootstrap` (`api/cmd/e2ai-api/main.go`, `api/internal/app/`) | v1.1 ships `0010_cockpit.sql` following `0001`–`0009` naming; no runner change. |

## New Dependencies

**None.** Explicitly rejected candidates:

| Candidate | Verdict | Reason |
|-----------|---------|--------|
| `@reduxjs/toolkit` / RTK Query | Reject for v1.1 | Module has no Redux; two paradigms in one module. Family-convention question → PROJECT.md decision note, separate milestone if ever. |
| `connectrpc` (Go) + `@connectrpc/connect` (web) | Reject for v1.1 | Zero existing usage; wire contract is locked `types.ts` ↔ Go DTOs. Would rewrite every surface or fork the contract. |
| `google/go-github` | Reject | `platform/github/client.go` is a deliberate hand-rolled client with rate-limit accounting; one more endpoint is ~30 lines. |
| SSE / socket.io / TanStack Query | Reject | `usePolling` + `api.ts` cover it; visibility-aware polling was built for exactly this ("report-only surface; avoid hammering a busy API", `usePolling.ts:1-4`). |
| Modal/dialog library | Reject | Two-step arm-confirm buttons match existing UX; admin-ui v0.6.0 component availability unverified — check before importing anything beyond `styles.css`. |

## Schema / Registry Surface (everything v1.1 must add)

1. **`0010_cockpit.sql`**: widen `runs_trigger_source_check` with `'manual'`; add `registered_apps.github_hook_id bigint` (+ optional `hook_created_at`); add a `manual_run_seq` sequence (starting 10^12) for synthetic workflow-run IDs.
2. **Registry config**: `github_hook_id` field in `registry/*.yaml` (`registry/bet-app.yaml`, `registry/betweb.yaml.example` gain the field); `webhook_secret_ref op://` unchanged per SECR-01.
3. **Permissions**: no new permission strings — `admin.e2ai.admin` gets its first real consumer (`httpx/auth.go:37`).

## Feature → Existing-Reuse Map (summary for the roadmapper)

| v1.1 Feature | Exists today | New work |
|--------------|-------------|----------|
| Cockpit trigger section | Ingest seed funnel, approvals machine, derive-draft endpoint, cost endpoint | `manual` trigger source + synthetic run IDs + trigger UI page |
| Webhook simulate | Ingest seam (transactional, idempotent) | Seed composer endpoint + UI |
| Webhook inbox | `webhook_events` + `webhook_triage` tables store full payloads; `ListTriage` query exists unmounted | Read surface (`GET /api/cockpit/webhook-events`, `/api/cockpit/triage`) + retry endpoint + inbox UI |
| GitHub redelivery | `platform/github` client | Hook-deliveries endpoint method + `github_hook_id` plumbing (secondary action) |
| Agent enqueue / kill | Fully implemented endpoints (`agents/http.go`) | Cockpit UI only |
| MCP probe | `POST /mcp` JSON-RPC | Probe action + response viewer |
| Sync jobs observe/control | River `JobList/JobCancel/JobRetry/QueuePause/QueueResume` in v0.47.0 already in go.mod | Thin cockpit handlers + jobs UI page |

## Risks / Flags for Other Researchers

- **JobCancel semantics:** River cancel of a *running* job is cooperative (job must observe context); runner-queue jobs (agent sessions) may need the existing kill switch instead. Verify per-job-kind behavior in the planning phase (PitfallsResearch: job-control foot-guns).
- **GitHub redelivery idempotency:** GitHub-side redelivery re-enters the normal webhook path — dedupe makes it safe (`unique (app_id, delivery_id)`), but repeated redeliveries of a *filtered* payload just re-record; document expected outcome in the UI copy.
- **`admin.e2ai.admin` is new blast radius:** first write actions gated on a never-before-used permission rung; staging verification of the BFF → header → permission chain for that rung is a phase acceptance criterion, not an assumption (BFF scope config lives in `config/admin-bff*.yaml` Okta scopes).

## Sources

- Repository code (all paths inline above; verified 2026-09-22): `web/package.json`, `web/admin.module.json`, `web/src/{app,lib,shared,features/**}`, `web/vite.config.ts`, `api/go.mod`, `api/cmd/e2ai-api/main.go`, `api/internal/{ingest,plans,approvals,agents,flakes,trends,mcp,report,platform/{httpx,github,river},app/migrations}`, `registry/*.yaml`, `config/admin-bff*.yaml`, `Tiltfile`.
- River v0.47.0 vendored source: `client.go` (`JobList`, `JobCancel`, `JobRetry`, `QueuePause`, `QueueResume`), `job_list_params.go`.

---
*Stack research for: e2ai v1.1 Admin Cockpit (@admin/e2ai), NSXBet shared-admin family — zero new dependencies, reuse-first*
