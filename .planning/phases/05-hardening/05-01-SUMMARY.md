---
plan: 05-01-PLAN.md
phase: 05-hardening
status: executed
executed: 2026-09-20
---

# 05-01 Plan Summary: Hardening — flake governance, roster completion, trends, MCP, multi-app

**Goal delivered:** the platform closes its P3 debt and v1 scope — flake rates with
quarantine governance, the full agent roster (Navigator/Vision Judge/Plan Author/Edge
Hunter/Flake Analyst) with plan-step consumption, agentic trajectories + evidence
citations in reports, trend analytics with alerting, a read-only MCP surface for coding
agents, scheduled bounded edge sweeps, selector-repair proposals behind a refuse-to-heal
gate, and config-only second-app onboarding.

## Waves executed

- **Wave 1 (T1, T2, T3)** — `ae6fb91d`: Go foundation — migration 0007 (flake/quarantine,
  trend, edge-sweep, ingest-secret, plan-session tables + `runs.trigger_source` widening),
  `flakes/` (rate reads + quarantine queue with guarded activate/lift), `trends/`
  (flip-rate, history, per-run agent cost), ingest secret-ref + AI triage hint, and the
  session-scoped runner-token `GET /api/agent-sessions/{id}/plan` (contract decision made
  mid-wave with T2: plan steps flow through the DB, not env). `78d47d2d`: agentrunner
  roster registry (`roles.ts` — five roles with prompts/budgets/permission hints, env
  overrides) + approved-plan step consumption in `api.ts`. `565cac66`: agentic trajectory
  + agent cost summary inside the report blob (additive DTO fields only).
- **Wave 2 (T4, T5, T6)** — `28cb4945`: edge sweeps (River-scheduled, opt-in apps only,
  findings land as learnings — never auto-promoted), Flake Analyst triggers, trend alerts
  (`/api/trends/alerts` with threshold/flip-rate kinds, dedupe cooldown, acknowledge).
  `da9cd6f5`: MCP JSON-RPC surface (`/mcp`, protocol errors -32700/-32600/-32601/-32602,
  five read-only tools: list_runs, get_run, get_failure_evidence, get_flake_rates,
  get_quarantine) — wired onto the mux in `6696549f` by the orchestrator (T4 owned
  main.go). `086aaa4`: admin web — AgenticTrajectory filmstrip on the report page,
  TrendsPage, QuarantinePage (activate/lift), CorpusHealthPage; types.ts exact mirror,
  i18n ×4 locales, manifest nav + commands validated.
- **Wave 3 (T7)** — `ae99afdc`: FLAK-02 selector-repair proposal flow — navigator
  suggestions from the roster → corpus repair proposals (`migration 0009`) as reviewable
  diff artifacts on the learning record, approval-gated, refuse-to-heal (confidence floor
  + error-page escalation) unit-tested on both sides of the wire. `dd85d311`: second-app
  onboarding — `registry/betweb.yaml.example` + README section; registry row + config,
  zero code (APPS-03 proven config-only).

## Verification evidence

- `go build ./...` clean; `go vet ./...` clean; `go test -p 1 ./...` — **all packages
  green** on the scratch postgres (localhost:55432; `-p 1` mandated by the plan's
  guardrail for the shared DB — parallel runs deadlock on the suite's schema locks).
- agentrunner: `tsc --noEmit` clean; `bun test` — **185/186 pass**; the single fail is
  `ios-smoke` (sim boot probe timeout — environmental, same class as phase 4's android
  smoke cold-boot flake; unit suites unaffected).
- web: `bunx tsc --noEmit` clean; `bun run build` clean (pre-existing chunk-size warning);
  `validate:manifest` OK (`@admin/e2ai`, routeBase `/e2ai`).
- **Live smoke** (scratch postgres, dev-fallback identity, `go run ./cmd/e2ai-api`):
  migrations 0001–0009 applied on boot; registry seeded (2 apps);
  `GET /api/flakes`, `GET /api/quarantine`, `GET /api/trends/flips|history|alerts` → 200
  with well-formed (empty-history) payloads; `POST /mcp` initialize → capabilities +
  serverInfo, tools/list → five read-only tools, `list_runs`/`get_flake_rates`/
  `get_quarantine` → structured content, unknown tool → `-32602`.
- **Auth wall:** without `DEV_FALLBACK_USER`, unauthenticated `/mcp` and `/api/flakes` →
  403; wrong bearer token → 403 (gate requires BFF RunViewPermission identity or runner
  service token).
- All five success-criteria clusters from the plan map to shipped surfaces (below).

## Requirements satisfied

AGNT-01, AGNT-03, AGNT-04, REPT-02, REPT-04, CORP-03, FLAK-01, FLAK-02, TREN-01, MCP-01,
EDGE-01, APPS-03 — marked complete in `.planning/REQUIREMENTS.md` (45/45 closed; v1 scope
complete).

## Deviations

- T2's session was aborted mid-run by the harness; its work landed (roles.ts + api.ts
  contract) and was verified + committed by the orchestrator. Same recovery for the wave-1
  T1 process — builds/tests re-run centrally before `ae6fb91d`.
- The contract question T2 raised (navigator plan-step source) was resolved mid-wave:
  session-scoped runner-token plan GET, mounted by T1 (`runner_data.go`), consumed by T2 —
  no new env vars, handoffs stay DB-only.
- MCP surface mount went in as an orchestrator commit (`6696549f`) because T4 owned
  `main.go` during wave 2 — kept the two workers file-disjoint.
- Device smoke flake (ios boot probe) left as-is: environmental, out of phase scope,
  unit suites green.
