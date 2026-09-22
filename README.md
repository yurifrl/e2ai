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

Prerequisites: **Go 1.26+**, **Node 24+**, **Bun**, **Docker**, and [Tilt](https://tilt.dev) with a local Kubernetes cluster (OrbStack, Rancher Desktop, kind — anything Tilt can drive).

```bash
tilt up
```

brings up:

- `postgres` on `localhost:5434`
- the admin BFF shell on `localhost:8080` (serves the UI)

The api runs as a bare process next to the stack (the BFF forwards `/e2ai/*` to it):

```bash
# .env at the repo root — the api auto-loads it (godotenv) at boot
cat > .env <<'EOF'
DATABASE_URL=postgres://e2ai:e2ai@localhost:5434/e2ai?sslmode=disable
DEV_FALLBACK_USER=you@example.com     # local-only identity; never set this in staging/prod
REGISTRY_DIR=./registry
E2AI_ARTIFACTS_DIR=/tmp/e2ai/artifacts
E2AI_REPORT_BASE_URL=http://localhost:8080/e2ai
E2AI_REPORT_LINK_SECRET=dev-link-secret
LOG_LEVEL=info
# GITHUB_TOKEN=…        # enables GitHub reconciliation + plan sync from a real repo
WEBHOOK_SECRET=local-dev-secret   # shared fallback when a registry op:// ref can't resolve
E2AI_RUNNER_SERVICE_TOKEN=dev-runner-token   # gates MCP + the runner lifecycle endpoints
EOF

cd api && go run ./cmd/e2ai-api   # listens on :8082
```

Env layering: `.env` base, then `.env.$ENV`, then `.env.$USER.local` (later layers override) — the same resolved env a pod sees via the `local/env-to-secret.sh` bridge.

On boot the api **applies all migrations** and **seeds the registry** from `registry/*.yaml`. Health check: `curl localhost:8082/healthz`.

Finally the UI module (Vite dev server; registered in the admin shell by `web/admin.module.json`):

```bash
cd web && bun install && bun run dev    # :3005
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
# secrets/webhook_payload.json is your workflow_run event JSON
BODY=$(cat secrets/webhook_payload.json)
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "local-dev-secret" -hex | sed 's/.* //')
curl -s localhost:8082/github/webhook \
  -H "Content-Type: application/json" \
  -H "X-GitHub-Event: workflow_run" \
  -H "X-Hub-Signature-256: sha256=$SIG" \
  -H "X-GitHub-Delivery: local-$(date +%s)" \
  -d "$BODY"
```

Ingest dedupes the delivery → plans sync → derive → the run enters `executing` and the
**deterministic suite** runs: one `docker run` of the suite container (clone → build →
Playwright suites → artifacts out).

**Agentic runs** are a separate, human-triggered layer on the same run — approve the plan,
then enqueue the agent roster (navigator, vision judge, …) with:

```bash
curl -s -X POST localhost:8082/api/runs/<runId>/agent-sessions
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
curl -s localhost:8082/mcp -X POST -H "Authorization: Bearer dev-runner-token" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

Tools: `list_runs`, `get_run`, `get_failure_evidence`, `get_flake_rates`, `get_quarantine`. Evidence links are token-gated and cite run ids — a PR session can pull the failing step's screenshot and verdict without dashboard access.

## 8. Developing the platform itself

```bash
cd api && go build ./... && go vet ./...

# Tests run a destructive migrate down/up — give them their OWN database
# (e.g. e2ai_test), never the dev database:
psql postgres://e2ai:e2ai@localhost:5434/postgres -c 'create database e2ai_test'
E2AI_TEST_DATABASE_URL=postgres://e2ai:e2ai@localhost:5434/e2ai_test?sslmode=disable \
  go test ./... -p 1 -count=1        # serial: the suite shares one scratch DB

cd ../agentrunner && bun install && bun run typecheck && bun run test
cd ../web && bun run build && bun run validate:manifest
```

The agentrunner suite includes real-device smokes (Android emulator, iOS simulator) that boot hardware — expect environmental flakes on cold boots; the Go/web suites are hermetic.

## Production posture

Staging config lives in `config/admin-bff.staging.yaml` (real Okta issuer, in-cluster Postgres); secrets resolve through 1Password references (`op://…`) via `local/env-to-secret.sh`. Set `E2AI_RUNNER_SERVICE_TOKEN` in any real environment — without it the api fails closed on runner endpoints. CI, container images for the api, and prod rollout are tracked platform work, not yet wired in-repo.
