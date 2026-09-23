# Architecture Research: v1.1 Admin Cockpit — Integration Points

**Domain:** Control-plane cockpit for an autonomous AI QA arm (triggering, observing, and operating an existing report-only QA pipeline)
**Researched:** 2026-09-22
**Mode:** Integration mapping (ecosystem mode, repo-grounded)
**Confidence:** HIGH — every integration point below is verified against the live repo (file:line citations). The only MEDIUM-confidence items are explicitly tagged (River running-job cancel propagation; replay identity minting).

## Ground Truth: Family Conventions vs Repo Reality

PROJECT.md's constraint names the shared-admin family stack ("React + RTK feature slices, hexagonal Go domains, Postgres, Connect-RPC, Tilt/K8s"). The e2ai repo **deliberately deviates on two of these**, verified in dependency files:

| Family convention | e2ai reality (verified) | Verdict for cockpit |
|---|---|---|
| Connect-RPC | `api/go.mod` has **no connectrpc module**. HTTP surface is plain `net/http.ServeMux` with JSON REST under `/api/*` (`api/cmd/e2ai-api/main.go` `newMux`, lines 327–421) | Follow repo reality: new cockpit endpoints are plain REST handlers. Do NOT introduce Connect-RPC in v1.1 |
| RTK feature slices | `web/package.json` has **no redux/@reduxjs**. Deps: `react@18`, `react-router-dom@^6.28`, `@nsxbet/admin-sdk@^0.14`, `@nsxbet/admin-ui@^0.6`, `tailwindcss@3`. State = per-page fetch via `web/src/lib/api.ts` + `web/src/lib/usePolling.ts` | Follow repo reality: cockpit pages are fetch + `usePolling`. Do NOT introduce RTK in v1.1 |
| Hexagonal Go domains | Matched exactly: each domain package owns its types, queries, workers, and an `*HTTP` surface struct with `Mount(mux)` | Matched: cockpit actions land in owning domains |
| River jobs, Postgres, Tilt/K8s | Matched (`api/internal/platform/river`, `postgres`, `Tiltfile`) | Matched |

The mismatch was already flagged in v1.0 research ("Connect RPC mention in doc comment is stale") and remains accurate. **This file treats the repo as normative.**

---

## 1. System Context Map

Two processes share one Postgres DB; River queues are process-disjoint (`api/internal/platform/river/river.go:1-4`):

```
GitHub webhook ──▶ admin-bff ──▶ e2ai-api (drains River "default" queue)
                                   │  ingest · plans · approvals · agents-http
                                   │  report · derive · flakes · trends · mcp
                                   ▼
                              postgres (app schema + river_* schema, shared)
                                   ▲
e2ai-runner (drains River "runner" queue: e2ai:run:execute + agent session worker)
                                   │ docker run (playwright) + agent loops
                                   ▼
                             bet-app under test (report-only: never mutated)
```

| Component | Responsibility | Cockpit relevance (v1.1) |
|---|---|---|
| `api/internal/ingest/` | Webhook handler, transactional ingest, wait-rollout gate, reconcile sweep | Manual trigger + webhook inbox/replay |
| `api/internal/suite/` | Execute job (runner), plan-resolution gate, canary | Manual-trigger blueprint |
| `api/internal/agents/` | Session orchestrator, budget engine, sweeps, AgentsHTTP | Agent enqueue/kill surfaces already exist |
| `api/internal/plans/` | Plan catalog, sync/corpus-seed jobs, PlansHTTP | Sync-job observation; approved-version resolution |
| `api/internal/report/` | RunsHTTP (list/detail/report/artifacts), report job, PR comments | Read models cockpit reuses |
| `api/internal/approvals/` | Approval queue + attributed decisions (Mount) | "Cockpit shows what's approved" reads this |
| `api/internal/platform/river/` | Queue-disjoint client builders | Job ops (observe/cancel/pause) |
| `api/internal/platform/httpx/` | BFF identity middleware + permission ladder | Every cockpit endpoint gates through it |
| `api/internal/mcp/` | Read-only JSON-RPC at `/mcp` | "MCP probe" admin action target |
| `web/src/features/*` | One folder per surface, page + `lib/api.ts` helper + `usePolling` | Cockpit features mirror this shape |

## 2. Trigger Path Today (verified end-to-end)

### 2.1 Sources enum

`runs.trigger_source` is a **check constraint**, not just a comment:

- `0001_init.sql` — column added with comment `webhook | reconcile` (no constraint).
- `0007_hardening.sql` (bottom) — the real gate:

```sql
alter table runs add constraint runs_trigger_source_check
  check (trigger_source in ('webhook', 'reconcile', 'sweep'));
```

Go constants mirror it (`api/internal/ingest/dedupe.go`): `TriggerWebhook`, `TriggerReconcile`, `TriggerSweep`. **A cockpit "manual" trigger requires a migration widening this check + a new constant.**

### 2.2 The five live entry points

| # | Trigger | Route/Kind | Entry file | Path |
|---|---|---|---|---|
| 1 | Deploy webhook | `mux.Handle("/github/webhook", webhook)` + `"/github/webhook/"` (`main.go:338-339`) | `ingest/http.go` `Handler` | HMAC verify (`ingest/secrets.go` op:// resolver + shared fallback) → match `app.App` (registry seeded at boot, `app/service.go` `Bootstrap` → `registered_apps`) → filter (workflow_name + display_title matchers) → `Store.Ingest` |
| 2 | Reconcile sweep | periodic `e2ai:ingest:reconcile` (default queue) | `ingest/sweep.go` `ReconcileWorker` | Lists recent completed `workflow_dispatch` runs via `platform/github` (`RunsLister`) → same `Store.Ingest` with `TriggerReconcile` |
| 3 | Edge sweep | periodic `e2ai:edgesweeps:tick` (default) | `agents/edgesweep.go` | Creates run row with `trigger_source='sweep'` + one `edge_hunter` session; findings land as corpus learnings |
| 4 | Image canary | periodic `e2ai:canary:image` (default) | `suite/canary.go` | **Direct run insert**: `trigger_source='reconcile'`, `run_number=0`, **synthetic negative `workflow_run_id`**, status `provisioning`, then enqueues `ExecuteArgs{Image: candidate}` on the runner queue |
| 5 | Agent session enqueue | `POST /api/runs/{runId}/agent-sessions` | `agents/http.go` `Enqueue` | Admin-only, see §2.4 |

### 2.3 The shared transactional spine (`ingest/dedupe.go`)

All webhook/reconcile runs commit atomically in one tx:

1. `webhook_events` insert — dedupe on `(app_id, delivery_id)` (`insertEvent`).
2. `runs` insert — dedupe on `(app_id, workflow_run_id)` (`insertRun`, on-conflict-nothing).
3. Wait-rollout job `InsertTx` — `e2ai:run:wait-rollout`, **default queue**, `MaxAttempts: 1`, `UniqueOpts.ByState` (scheduled/pending/available/running) so webhook-vs-sweep races never double-enqueue.

Then `WaitRolloutWorker` (`ingest/rollout.go`): guarded transition `queued → waiting-rollout`, polls the app's `health_url` until 2xx (fallback: fixed 5m), then enqueues `ExecuteArgs` on the **runner** queue (`rollout.go:48`). Never-healthy → verdict INFRA_ERROR + guarded `→ failed` (one-shot gate, returns nil so River does not retry).

`Executor` (`suite/execute.go`, runner process): `provisioning → executing` (guarded) → neutral check-run `in_progress` → `exec.CommandContext(ctx, "docker", ...)` clone/build/serve/playwright (`execute.go:127`) → parse results → upsert `run_plan_resolutions` via `resolvePlanGate` (`execute.go:370-518`) → hand report job back to the **default** queue (`e2ai:run:report`, `execute.go:76`) → `executing → reporting`. `report/job.go` (api process) finalizes verdicts, check-run, PR comment.

**Plan-resolution gate** (`run_plan_resolutions`, `0003_derive.sql`): `mode ∈ approved | legacy_full_suite | draft_only`. `draft_only` fails the run — APPR-01 forbids executing without approved expectations. The gate writes `{plan_id, plan_version_id, content_hash, selected[], mode}` — the record a manual trigger must produce or bypass-with-equivalent.

### 2.4 The two admin-facing write paths that already exist

- **Agent enqueue** (`agents/http.go` `Enqueue`, `POST /api/runs/{runId}/agent-sessions`): gates on `httpx.RunEditPermission`; run must be `executing` (else 409); requires `run_plan_resolutions r join plan_versions pv where r.mode='approved' and pv.status='approved'` (else 409); inserts one `navigator` + one `vision_judge` session with `step_ceiling = len(plan.Steps)`; re-enqueue = 409.
- **Derive-draft re-trigger** (`main.go:405-421`, `POST /api/runs/{id}/derive-draft`): `RunEditPermission` → `riverClient.Insert(derive.DeriveDraftArgs{RunID, PRNumber: 0})` → `202 {"enqueued":true}`. **This is the exact HTTP shape a cockpit trigger endpoint copies.**

---

## 3. Manual Trigger — Exact Integration Recipe

Closest existing analog: `suite/canary.go` (creates a run outside the webhook path) × the derive-draft endpoint (admin-gated POST that enqueues a job).

Required integration points, in dependency order:

1. **Migration** (`api/internal/app/migrations/0010_*.sql`): widen `runs_trigger_source_check` to include `'manual'` (0007 shows the exact constraint text). No other table change — `runs` already carries `head_sha`, `head_branch`, `environment`.
2. **Constant** (`ingest/dedupe.go`): `TriggerManual = "manual"` alongside `TriggerWebhook | TriggerReconcile | TriggerSweep`.
3. **Endpoint** (new handler in `ingest`, mounted in `newMux` — `ingest` already owns run creation via `ingest.Store`; do not invent a new package for one endpoint):
   - `POST /api/cockpit/triggers` body `{app_id, head_sha, head_branch?, environment}`; permission `httpx.RunEditPermission` (mirror `agents/http.go` gating style).
   - Resolve the app row from `registered_apps` (seeded from `registry/*.yaml` at boot — `app.Bootstrap`). Reject unknown app 404.
   - Resolve `head_sha`: accept an explicit SHA, or a branch resolved via the `platform/github` client (already wired for plans/reconcile). Empty SHA → 400.
   - One tx (mirror `ingest/dedupe.go` `Store.Ingest`): insert run row `trigger_source='manual'`, `status='provisioning'`, `run_number` next per app, `workflow_run_id` = **synthetic negative value reusing the canary scheme** (`suite/canary.go` `ensureRun` — keeps `(app_id, workflow_run_id)` dedupe collision-free), then `riverClient.Insert(ExecuteArgs{RunID}, canaryExecuteOpts-style opts)` — runner queue, `MaxAttempts: 1`, `UniqueOpts.ByState` dedupe.
4. **Approval envelope**: do NOT bypass APPR-01. If the app has no approved plan version anywhere → 409 (the same condition `resolvePlanGate` treats as `draft_only`-fail). Minimal v1.1: the manual run resolves plans through the executor's existing `resolvePlanGate` unchanged; pinning one specific `plan_version_id` requires extending `ExecuteArgs` — flag for the planner.
5. **Pitfall**: cockpit "retry" must NOT re-run an old execute job. Terminal run states have no outgoing edges (`runs/state.go` `validTransitions`); the execute job is `MaxAttempts:1` → discarded; `JobRetry` on it re-enters the executor, whose guarded `executing` transition from `failed` is a typed rejection → job errors again. Cockpit retry = **fresh manual run** (same head SHA, same plan), never a job-level retry.

---

## 4. River Job Observation (verified against riverqueue/river v0.47.0, module cache)

Open-source River (what the repo pins: `river v0.47.0` in `api/go.mod`) exposes the full inspection/control surface — no Pro dependency needed:

| API | Semantics |
|-----|-----------|
| `JobList(ctx, *JobListParams)` / `JobListTx` | Filter by `States`, `Queues`, `Kinds`, `IDs`, `Metadata`, `TagsAll/TagsAny`; `OrderBy`, `First`, `After` cursor pagination (`job_list_params.go`) |
| `JobGet(ctx, id)` | Single job row |
| `JobRetry(ctx, id)` | Returns a `discarded`/`cancelled` job to `available` — errors otherwise |
| `JobCancel(ctx, id)` | Pending states cancel immediately; **running** jobs are marked + a control event is NOTIFYed to the owning client (`client.go` `JobCancel` → `notifyProducerWithoutListenerQueueControlEvent`) |
| `JobUpdate(ctx, id, JobUpdateParams{State, Error, Metadata, Output})` | Manual state nudges |
| `QueuePause` / `QueueResume` / `QueueList` | DB-level pause on `river_queue`; affects every subscribing process |

Job-kind inventory (all verified strings):

| Kind | Package | Queue | Worked by |
|------|---------|-------|-----------|
| `e2ai:plans:sync` | `plans/sync.go:46` | default | api |
| `e2ai:corpus:seed` | `plans/sync.go:47` | default | api |
| `e2ai:ingest:reconcile` | `ingest/sweep.go:23` | default | api |
| `e2ai:run:wait-rollout` | `ingest/http.go:30` | default | api |
| `e2ai:derive:draft` | `derive/job.go:36` | default | api |
| `e2ai:run:execute` | `suite/execute.go:45` | **runner** | runner |
| `e2ai:run:report` | `suite/execute.go:50`, `report/job.go:38` | default | api |
| `e2ai:canary:image` | `suite/canary.go:50` | default | api |
| `e2ai:agent:sweep` | `agents/sweep.go:34` | default | api |
| `e2ai:edgesweeps:tick` | `agents/edgesweep.go:43` | default | api |
| flakes tick | `agents` (periodic, `main.go:148`) | default | api |

Periodic jobs are configured with `river.NewPeriodicJob` in `main.go:158+` (reconcile sweep, orphan sweep, edgesweeps tick, flakes tick, canary). Cockpit recommendation: read via `riverClient.JobList` (Kinds filter `e2ai:*`, `First`/`After` pagination) on the api's existing client — zero new dependencies. Direct SQL on `river_job` is acceptable for counts only (tests already do it); prefer the API for state correctness.

---

## 5. Control Safety Analysis (queue-disjoint clients)

**Queue topology** (`platform/river/river.go`): the api container drains only `default`; the runner container drains only `runner` (`RunnerClient` builder). Inserting a runner-queue job from the api is already the norm (rollout handoff `ingest/rollout.go:48`, canary `canaryExecuteOpts`), and job control ops (cancel/retry/pause) are DB-level and process-agnostic.

**Cancel a running execute (runner queue):** `JobCancel` from the api → row marked cancelled + control NOTIFY → the runner's client cancels that job's work ctx → `exec.CommandContext` docker run is killed (`suite/execute.go:127`) → the executor's `noteFail` guarded transition `executing → failed` lands with `error_note`. Chain verified piecewise; the runner client's listener config should be confirmed in-phase (confidence MEDIUM end-to-end, HIGH per-link).

**Cancel is not enough for the run row:** a cancelled execute job leaves the run in `queued`/`waiting-rollout`/`provisioning`. A cockpit cancel action must cancel the job **and** move the run to terminal via a guarded `Transition` (`runs/state.go` — the guard rejects concurrent drift). `Transition` has no `error_note` param; the cockpit needs a small runs-package helper that stamps `error_note` then transitions (like `suite`'s `noteFail`).

**Retry asymmetry:** job-level retry is legitimate only for default-queue kinds — `e2ai:run:report` (a failed report leaves the run stuck in `reporting`; retrying the job is the correct ops lever) and `e2ai:plans:sync`/`e2ai:derive:draft`. It is **never** the retry path for `e2ai:run:execute` (see §3.5 — fresh run instead).

**Queue pause blast radius:** `QueuePause('runner')` halts every app's execute; `QueuePause('default')` halts wait-rollout (runs pile up in `waiting-rollout` forever) and periodic ticks accumulate. Pause persists in `river_queue` across restarts until resumed. Gate both behind `httpx.RunAdminPermission`, surface current pause state in the UI, and label persistence explicitly.

**Invariants preserved:** all cockpit controls operate on the QA arm's own control plane — they never mutate the app under test (report-only posture holds). Session-level control already exists and is not duplicated: kill switch `POST /api/agent-sessions/{id}/kill` (admin) and the budget engine's terminal `budget_exhausted` (`agents/budget.go`).

---

## 6. Web Integration (verified — and a deliberate deviation from family convention)

**Ground truth:** the repo uses **plain net/http REST + fetch**, not Connect-RPC, and **no RTK**. `api/go.mod` has no connectrpc module; `web/package.json` deps are `react 18.3`, `react-router-dom 6.28`, `@nsxbet/admin-sdk 0.14`, `@nsxbet/admin-ui 0.6`, `tailwindcss 3.4` — no redux. The family conventions named in the milestone brief (Connect-RPC, RTK slices) do NOT match this repo; cockpit endpoints/pages must follow the repo's actual patterns (a second convention beside the existing one is prohibited).

### Manifest & navigation
- `web/admin.module.json`: `id @admin/e2ai`, `navigationOrder 50`, `navigation.style "stacked"` with one section `operations`; `commands[]` entries (runs, plans, approvals, corpus, quality, trends, corpus-health) each carry i18n titles (en-US, pt-BR, es, ro) + icon + section. Cockpit adds a command entry (and, if grouping is wanted, a second section) here — plus the matching `permissions` block entry.
- `web/src/app/routes.tsx`: react-router children under the module base `/e2ai` — add a `cockpit` route next to `approvals`, element `CockpitPage` in `web/src/features/cockpit/`.

### Gateway & identity
- `web/src/shared/gateway.ts` `e2aiApiBase()`: `VITE_E2AI_API` → `env.adminGatewayUrl` → dev fallback `http://localhost:8080` → `''`. All module calls go `${base}/e2ai/api/*`; the BFF strips the `/e2ai` prefix and forwards to e2ai-api (`config/admin-bff.yaml`: backend `e2ai-api`, `host.docker.internal:8082`, router prefix `/e2ai`).
- `web/src/shared/authenticatedFetch.ts` + `AuthenticatedFetchProvider` carry BFF identity; the api reconstructs it in `platform/httpx/auth.go` from `X-User` / `X-Permissions` (base64 JSON). Server-side gating for new endpoints: `admin.e2ai.edit` (`RunEditPermission`) for triggers, `admin.e2ai.admin` (`RunAdminPermission`) for cancel/pause — exact strings in `httpx/auth.go:35-37`.

### Data layer & actions
- `web/src/lib/api.ts`: typed helpers over `getJson`/`postJson` (via `authenticatedFetch`). Add `triggerRun`, `listRiverJobs`, `cancelRiverJob`, `retryRiverJob`, `setQueuePaused` — same naming/export shape as `listRuns`.
- Polling, not RTK Query: `usePolling` (visibility-aware, debounced tick) drives lists; `RunsListPage` auto-refreshes the first page every 10s while visible — cockpit lists copy this.
- POST-action precedent to mirror: the approvals queue decisions (`ApprovalsQueuePage` → `lib/api.ts` → `ApprovalsHTTP` decide endpoints) — trigger buttons get the same confirm-then-post treatment.
- No Redux provider exists (`web/src/AppProviders.tsx`) — do not introduce one.

## 7. Anti-Patterns for the Cockpit (repo-specific)

| Anti-pattern | Why bad | Instead |
|---|---|---|
| Adding Connect-RPC or RTK for cockpit | Second convention beside the repo's REST+fetch; rejected in v1.0 research | Extend `lib/api.ts` + REST handlers |
| Flipping a terminal run back to `executing` | `runs/state.go` guard rejects it; evidence trail lies | Fresh manual run (§3.5) |
| JobRetry on an execute job | Re-enters executor against a terminal run → typed rejection → job re-errors | Retry only default-queue kinds; re-trigger runs for execute |
| Cancelling the job but not the run row | Run row stranded in a non-terminal status forever | Cancel + guarded transition in one endpoint |
| Bypassing `Transition` for any status change | Loses concurrency safety + terminal stamps | Always through the runs package guard |
| Browser calling runner endpoints | Runner endpoints are service-token gated (`E2AI_RUNNER_SERVICE_TOKEN`) | All cockpit calls are BFF-identity only |
| Cockpit mutating the app under test | Report-only invariant | Cockpit controls the QA arm only |

## 8. Operational Notes

- Conditional mounting: surfaces are `nil` when the pool is down and skipped in `newMux` (`main.go:344+` pattern); `/healthz` flips to degraded. Cockpit endpoints follow the same posture.
- River stop is bounded (`main.go`, 10s drain); the api client enqueues the whole job chain even though runner-queue jobs are worked by another process (`river.go:56`).
- Local wiring: api on `:8082`, web dev on `:3005`, admin-bff proxies `/e2ai` (`config/admin-bff.yaml`, `Tiltfile`).

## 9. Phase-Specific Warnings

| Cockpit topic | Likely pitfall | Mitigation |
|---|---|---|
| Manual trigger | `(app_id, workflow_run_id)` dedupe collision on synthetic IDs | Reuse canary's negative-ID scheme exactly |
| Manual trigger | Triggering while a webhook run for the same SHA is active | Return the existing active run (read `runs` by app+head_sha) instead of duplicating |
| Job ops | Cancel semantics differ per job state | Show River job state in UI; cancel only when job not terminal |
| Queue pause | Pausing `default` silently stalls the whole pipeline | `RunAdminPermission` + persistent-pause banner |
| Webhook replay/triage | `webhook_triage` is append-only upsert (`ingest/triage.go`); promotion touches registry seeding | Triage surface is read-only today; promotion design (registry YAML is config-as-code, seeded at boot via `app.Bootstrap`) is an open phase decision |
| MCP probe | `mcp.Surface` (`POST /mcp`) is JSON-RPC, not REST | Wrap, don't reimplement — cockpit calls through existing surface |

## 10. Sources

- `api/cmd/e2ai-api/main.go` — boot, workers, periodic jobs, `newMux` route table (lines 338-421)
- `api/internal/ingest/{http,dedupe,rollout,triage,sweep}.go` — webhook + reconcile spine, trigger constants
- `api/internal/runs/{state,queries}.go` — guarded status machine, run shape
- `api/internal/suite/{execute,canary}.go` — executor, canary (manual-trigger analog), queue opts
- `api/internal/agents/{http,sessions,budget,sweep,edgesweep}.go` — sessions, enqueue gate, kill switch
- `api/internal/plans/{sync,http}.go`, `api/internal/approvals/` — plan sync + approval machine
- `api/internal/app/migrations/0001,0003,0007` — schema, resolutions, trigger-source check constraint
- `api/internal/platform/{river/river.go,httpx/auth.go}` — queue topology, identity/permissions
- `web/admin.module.json`, `web/src/app/routes.tsx`, `web/src/lib/api.ts`, `web/src/shared/{gateway,authenticatedFetch}.ts`, `web/package.json`
- River v0.47.0 source (module cache): `client.go` (`JobCancel/JobRetry/JobList*`), `job_list_params.go`
- All claims repo-grounded (HIGH) unless tagged; River API semantics from upstream source (HIGH); end-to-end cancel propagation MEDIUM (listener config unverified).
