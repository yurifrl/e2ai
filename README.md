# e2ai

Autonomous e2e testing control plane: **declarative plans + a governed agent roster**
(navigator, vision_judge, edge_hunter, flake_analyst) that runs them against real
web/android/ios surfaces, verifies against a human-owned corpus oracle, and reports.

**Report-only by design.** The platform never mutates the app under test, never blocks
a deploy, and every agent proposal (heal, repair, quarantine) is human-gated. Agents
propose; humans decide.

```
GitHub deploy ──▶ api (Go control plane) ──▶ suite container (Playwright, deterministic)
                       │  plans → approvals → approved-only execution
                       ├─▶ agentrunner (TS roster: navigator + vision_judge …)
                       ├─▶ report: verdict + evidence filmstrip + PR comment
                       └─▶ flakes / trends / quarantine / MCP (read-only)
```

| Directory | What lives there |
|---|---|
| `api/` | Go control plane: plans, approvals, corpus, flakes, trends, agents, report, MCP |
| `agentrunner/` | TS agent loop + roster registry + surface drivers (web/android/ios) |
| `web/` | Admin module UI: runs, report (trajectory), trends, quarantine, corpus |
| `registry/` | Per-app declarative config (`*.yaml`) |
| `runner/` | Suite container image used for deterministic runs |
| `docs/` | Plan format spec and other contracts |

---

## 1. Start it

Prerequisites: **Go 1.26+**, **Node 24+**, **Bun**, **Docker**, [Tilt](https://tilt.dev) with a local Kubernetes cluster (OrbStack, Rancher Desktop, kind — anything Tilt can drive), the **1Password desktop app** (signed in), and [go-task](https://taskfile.dev). Every command below is wrapped in the repo's `Taskfile.yml` — `task --list` shows the surface, and each task's commands are right there in the file when you want to see exactly what runs.

```bash
tilt up
```

brings up:

- `postgres` on `localhost:5434`
- the admin BFF shell on `localhost:8080` (serves the UI)

The api runs as a bare process next to the stack (the BFF forwards `/e2ai/*` to it). Its environment is **declarative**: `secretspec.toml` commits only the declarations — values live in 1Password (company account `nsx-team.1password.com`, vault `SRE Team`, item `e2ai`) and resolve at process start via [SecretSpec](https://secretspec.dev) with the `onepassword://nsx-team.1password.com@SRE Team` provider:

```bash
task secrets:check   # every declared secret must resolve
task api             # == secretspec run -- go run ./cmd/e2ai-api  (listens on :8082)
```

Env layering: secretspec injects the manifest values first, then the api layers `.env.$ENV`, then `.env.$USER.local` on top (later layers override) — personal overrides still win, and the same layer files feed the `local/env-to-secret.sh` bridge pods consume. Non-secret config (DB URL, ports, log level) needs no file: code defaults + manifest defaults cover local dev. CI uses `OP_SERVICE_ACCOUNT_TOKEN` instead of the desktop app.

On boot the api **applies all migrations** and **seeds the registry** from `registry/*.yaml`. Health check: `curl localhost:8082/healthz`.

Finally the UI module (Vite dev server; registered in the admin shell by `web/admin.module.json`):

```bash
task web    # == bun install + bun run dev in web/  (Vite dev server, :3005)
```

Open **http://localhost:8080/e2ai** — the Runs page. Empty until your first run.

## 2. Register your app

One yaml file, zero code. Copy the template:

```bash
cp registry/betweb.yaml.example registry/myapp.yaml
```

```yaml
owner: acme
repo: checkout
workflow_name: deploy-web
workflow_path: .github/workflows/deploy-web.yml
environment: staging
display_title_pattern: "[Checkout] Deploy to"
surfaces: [web]
plans_source:
  kind: repo_path
  path: e2ai/plans
webhook_secret_ref: op://Development/github-webhook-secret   # must be an op:// ref; if it can't resolve, the api falls back to the shared $WEBHOOK_SECRET
comment_marker: "<!-- e2ai:run:checkout-web -->"
```

Field names mirror the `registered_apps` DDL one-to-one; a malformed file is a hard boot error (config-as-code). `tilt up`/api restart re-seeds. That's the whole onboarding — second app, same story (APPS-03).

## 3. Write a test

A test is a **markdown plan file** in the app repo (full spec: [`docs/plans/format.md`](docs/plans/format.md)). Frontmatter tells the scheduler when/where; numbered steps tell the navigator what to drive and the judge what to verify. Every step carries a **falsifiable `judge:`** — a checkable predicate (numeric bound, state verb, or corpus reference) — and unverifiable phrasing ("the flow works nicely") is rejected at parse time.

```markdown
---
id: checkout-happy-path-v1.0.0
app: checkout-web            # matches your registry/<slug>.yaml
title: Checkout happy path (guest)
surfaces: ["web"]
trigger: on-deploy
---

## Steps

1. Open the storefront and search for "running shoes".
   judge: the page shows a result grid with at least one product card
2. Add the first result to the cart.
   judge: the cart badge shows "1"
   on-failure: continue
```

The key set is **closed** — a typo'd frontmatter key is a parse error at ingest, not a silently-unscheduled plan. Steps may also reference verified corpus behaviors via `uses: MATCH-LIVE-BADGE`; each key must resolve to a `verified` entry in the corpus or validation fails. The navigator is locator-first (accessibility-tree evidence), falls back to Stagehand vision, and a separate judge session verifies each clause before a verdict is written.

**Identity is content-hash based:** any byte change to an approved plan is a *new version* that lands in the approval queue. Nothing runs until a human approves it, and the run pins the exact approved version.

## 4. See it running

**Trigger.** `on-deploy` plans fire from a GitHub `workflow_run` (deploy completed) webhook delivered to the api. For local iteration without GitHub, fire the same webhook yourself with a valid HMAC signature:

```bash
task webhook:simulate   # signs secrets/webhook_payload.json with the vault WEBHOOK_SECRET
```

Ingest dedupes the delivery → plans sync → derive → the run enters `executing` and the
**deterministic suite** runs: one `docker run` of the suite container (clone → build →
Playwright suites → artifacts out).

**Agentic runs** are a separate, human-triggered layer on the same run — approve the plan,
then enqueue the agent roster (navigator, vision judge, …) with:

```bash
task agent:enqueue RUN_ID=<runId>
```

The gate is strict: the run must be `executing`, its plan resolution must select an
**approved** plan version, and the caller needs admin identity (locally `DEV_FALLBACK_USER`
provides it). Each session spawns `agentrunner` with a strict env contract
(`E2AI_SESSION_ID`, `E2AI_ROLE`, step/cost ceilings, …) — the runner never sees app
secrets (boot refuses them). Re-enqueueing is idempotent-safe (409 if sessions exist).

**Watch it live.**

- Runs list → run detail → **report page**: per-step verdicts, agent trajectory filmstrip, evidence citations. A missing citation renders `INCONCLUSIVE`, never a fake PASS.
- Artifacts are served at `/artifacts/{runID}/…` — screenshots, a11y snapshots, traces.
- Trends (`/api/trends/*`), flake rates and the quarantine queue show up as history accumulates.

## 5. Iterate

1. **Change a plan** (a judge clause, a step, a path) → the edit becomes a new pending version in the **Approvals queue**.
2. Approve it (or leave it — only approved versions execute; the run's plan resolution cites the approved content hash).
3. Re-trigger and compare: the report links the exact plan version that ran.
4. Something flaked? The flake analyst proposes a finding; **quarantine** a known-flaky test from the queue UI (report-stage skip signal, never a blind re-run); lift it when fixed. Selector repairs land as **reviewable diff proposals** on the learning record — accept them yourself.

## 6. Add prints (debugging your test)

The evidence pipeline *is* the print statement:

- **Screenshots per step.** The navigator captures `step-<n>.png` plus a11y snapshots under `$E2AI_ARTIFACTS_DIR/<run>/agents/<session>/…` — automatically, for every step. The judge cites those artifacts by path in the report, so every verdict links its evidence; make evidence richer by phrasing sharper judge clauses:

  ```markdown
  2. Add the first result to the cart.
     judge: the cart badge shows "1" and the drawer lists the running-shoes item
  ```
- **Agent loop logs.** `agentrunner` writes progress to stderr; the api streams that into the runner log and keeps the tail as the session's failure note — open the session in the report to read exactly what the agent saw.
- **Control plane logs.** `LOG_LEVEL=debug` when launching the api turns on request-level logging with secrets redacted.
- **Deterministic runs** keep full Playwright output (traces, videos, screenshots) in the same artifacts tree.

## 7. Query it from a coding agent (MCP)

The api exposes a **read-only** MCP surface on `POST /mcp` (JSON-RPC, Bearer-token or BFF identity gated):

```bash
task mcp:probe   # resolves the runner token from 1Password, lists MCP tools
```

Tools: `list_runs`, `get_run`, `get_failure_evidence`, `get_flake_rates`, `get_quarantine`. Evidence links are token-gated and cite run ids — a PR session can pull the failing step's screenshot and verdict without dashboard access.

## 8. Developing the platform itself

```bash
task testdb          # once: creates the e2ai_test scratch database
task api:test        # destructive migrate — the suite never touches the dev database
task api:build       # go build ./... && go vet ./... in api/
task agentrunner:test
task web:build
```

The agentrunner suite includes real-device smokes (Android emulator, iOS simulator) that boot hardware — expect environmental flakes on cold boots; the Go/web suites are hermetic.

## Production posture

Staging config lives in `config/admin-bff.staging.yaml` (real Okta issuer, in-cluster Postgres); secrets resolve through 1Password references (`op://…`), and the env-to-secret bridge (`local/env-to-secret.sh`) merges the `.env.$ENV` layer files for pods. `E2AI_RUNNER_SERVICE_TOKEN` must resolve in any real environment — without it the api fails closed on runner endpoints. CI, container images for the api, and prod rollout are tracked platform work, not yet wired in-repo. The runner-image canary stays inert unless `E2AI_CANARY_IMAGE` (optionally `E2AI_CANARY_APP`) is set — semantics documented on the constants in `api/internal/suite/canary.go`.
