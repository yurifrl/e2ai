# Phase 3: Agent roster & the agentic loop — Research

**Phase:** 03-agent-roster-agentic-loop · **Researched:** 2026-09-20
**Confidence:** HIGH on landed-code + in-house-repo facts (aihub-gateway README/ADR read this session); MEDIUM-HIGH on runner-service design (follows verified Paperclip/control-plane patterns from .planning/research/ARCHITECTURE.md).

## Platform grounding (landed, Phase 1+2)

- Go API: `api/cmd/{e2ai-api,e2ai-runner}`, packages `ingest/ runs/ plans/ approvals/ corpus/ derive/ suite/ report/ verdict/` — REST net/http JSON, BFF identity via `httpx.IdentityWithConfig`, River queues `default` (api) + `runner` (runner, MaxWorkers 1), scratch-postgres test pattern `E2AI_TEST_DATABASE_URL` + `app.Migrate`/`app.MigrateRiver`.
- Runs state machine: queued→waiting-rollout→provisioning→executing→reporting→completed|failed (`runs/state.go`, guarded transitions).
- Plans/corpus: `plan_versions.status` draft|pending_approval|approved|superseded; execution gate selects approved-only by content hash; corpus `corpus_behaviors` (verified baselines, provenance jsonb) is the Vision Judge oracle.
- Report surface: `/api/runs/{id}/report` + artifacts; verdict vocabulary PASS/FAIL/FLAKY/INFRA_ERROR/INCONCLUSIVE.
- Runner container: `runner/Dockerfile` (mcr playwright base) for deterministic runs — the agent runner is a SEPARATE service (below), not a fork of this.

## Decision log (each with resolution)

| Q | Resolution | Fallback |
|---|---|---|
| Where does the agent loop run? | **TS service** (`agentrunner/`, Node 24, TS): Playwright+Stagehand are TS-native (STACK decision, verified). Control plane stays Go. | Go-only loop would drop Stagehand/trace ecosystem — rejected in STACK.md |
| How do TS agents integrate with the Go control plane? | Go runner worker (river `agent` queue) spawns one `agentrunner` process per session with env (session id, api base, gateway token ref); process drives loop, appends steps + heartbeats over HTTP; dies with its session (ARCHITECTURE Pattern 1: agents ephemeral, state in Postgres) | Long-poll claim API if process-per-session proves heavy at scale |
| Gateway contract? | **aihub gateway** (in-house repo verified): `POST {AIHUB_BASE_URL}/v1/chat/completions`, `Authorization: Bearer gt_...`, OpenAI-compatible request/response incl. `usage`. Vision messages = content array with `image_url` data URLs | If aihub adds a runs-budget endpoint later, switch budget key issuance to it |
| "Gateway-level per-run budget keys" (BUDG-01)? | aihub budgets are **per internal user**, not per token (ADR-0002 verified) -> per-run key = per-run service sub-user is over-engineering for v1: runner accumulates per-run cost from `usage` fields (prompt+completion tokens x model price table) and hard-stops at cap; gateway-side cap = dedicated e2ai service user budget set in aihub admin (ops config, documented in .env.op) | aihub per-run keys when the gateway ships them |
| Navigation layer? | Playwright locators + `page.accessibility.snapshot()`/ariaSnapshot grounding primary; Stagehand `act()` for NL fallback actions (STACK: v4.0.0 act/extract/observe). Vision never clicks blind: every action logs pre-action a11y snapshot + locator | Raw CDP if Stagehand fights the pinned Playwright |
| Vision Judge independence? | Separate session (separate process, separate chat history — never shares Navigator context), different gateway model routing (`vision_judge` -> strong VLM; `navigator` -> fast flash-class). Role->model map in config, per-role system prompts | Same-family different-context if a single model family serves both — independence comes from context first |
| Oracle? | `corpus_behaviors` (verified baselines) fetched per plan step `uses:` key; deterministic assertions (URL, selector state, HTTP status) run in-process alongside VLM judgment; VLM verdict without machine-checkable evidence = `inconclusive` (AGNT-04, REPT-04) | none — this is a hard requirement |
| Session schema? | `agent_sessions` (per run x role, status machine, worker_id, heartbeat_at) + `session_steps` (append-only: idx, kind, decision json, action json, evidence refs, tokens_in/out, cost_millicents, created_at) + budget columns on sessions (steps_used, cost_millicents, step_ceiling, cost_ceiling) | separate budget ledger table if per-step cost rows needed for trends (P5) |
| Orphan recovery? | River periodic job: sessions `claimed|running` with `heartbeat_at` older than 90s -> requeue fresh session (append-only log keeps the dead attempt), `error_note` orphaned | none |
| Kill switch? | `POST /api/sessions/{id}/kill` (RunEditPermission): sets status=killed; agent's next heartbeat/step-append gets 409 and exits non-zero; stuck detection = same heartbeat lapse threshold drives a `stuck` flag on the admin queue page | SIGTERM escalation if a hung Playwright ignores 409 |
| Sandbox (SAFE-01)? | agentrunner runs in a dedicated container: no credentials mounted (only gateway token + e2ai API token, both scoped to e2ai), egress allowlist = staging domains + aihub + api; outbound proxy enforcement via container network policy (documented iptables/dns allowlist in runner compose/tilt); page content is data: prompts wrap screenshots/a11y in explicit data-fences; full trajectory = session_steps + artifacts | host-level network namespace if container policy proves porous |
| REPT-04? | Report page + `/api/runs/{id}/report` gain `trajectory`: ordered session_steps with evidence citations (artifact URLs); verdict rendering: any vision verdict lacking machine-checkable evidence renders INCONCLUSIVE (verdict package rule extension) | none |

## Roster roles (AGNT-01) — what each does in P3

| Role | Kind | Job | Bounded by |
|---|---|---|---|
| navigator | run | execute approved plan steps on web surface | plan step list + budgets |
| vision_judge | run | verify each navigator step against corpus oracle + deterministic assertions | same session budget pool |
| plan_author | on-demand | draft a plan from corpus behaviors into approvals queue (human approves in P2 surface) | 1 draft per request |
| edge_hunter | on-demand | bounded exploration (<= N steps, budget-capped) on staging; observations land as corpus learnings | hunt ceiling |
| flake_analyst | on-demand | analyze verdict-flip history for a plan; produce stability note (learning) | read-only + 1 note |

Governed handoffs: every inter-agent artifact passes through the DB (sessions/steps/corpus), never in-process memory. Navigator -> Vision Judge: step rows. Hunter -> corpus: learnings (unverified). Author -> approvals: draft plan_version.

## Code examples (verified shapes)

### Gateway call (aihub, OpenAI-compatible)
```bash
curl -X POST "$AIHUB_BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $AIHUB_GATEWAY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"...","messages":[{"role":"user","content":[{"type":"text","text":"..."},{"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}]}]}'
```
Response carries `usage.prompt_tokens/completion_tokens` (cost accumulator source). Verified from in-house README this session.

### Session claim (atomic, Go)
```sql
update agent_sessions set status='claimed', worker_id=$1, claimed_at=now()
where id = (select id from agent_sessions where status='queued'
            order by created_at for update skip locked limit 1)
returning *;
```

### TS loop skeleton
```ts
while (await budget.ok() && !killed) {
  const snap = await page.accessibility.snapshot(); const shot = await page.screenshot();
  const decision = await gateway.chat(role.model, judgePrompt(snap, planStep)); // JSON action
  const evidence = await act(page, decision);                                   // locator-first
  await api.appendStep(sessionId, { idx, decision, evidence, usage });
  await api.heartbeat(sessionId);                                               // 409 => killed
}
```

## Pitfalls (from ARCHITECTURE/PITFALLS, phase-relevant)

- Deploy != live is Phase 1's rollout gate — agentic runs inherit it (runs already gate).
- Page content is DATA never instructions: fence every screenshot/a11y payload in prompts; system prompt states injection-resistance (SAFE-01).
- Vision plateaus on dense UIs: a11y grounding is primary (Pattern 4, verified).
- Cost: flash-class for navigation, strong VLM for verdicts (STACK: per-role routing decision).
- Non-retryable semantics: BudgetExhausted and killed must NOT be retried by River (MaxAttempts 1 + terminal status checks).
- Playwright blob artifacts from the agent loop must land in the same artifacts root as deterministic runs (report reuse).
- Heartbeat every step AND before long actions; 90s lapse threshold beats any legit long step (screenshot+LLM < 30s typical).

## DDL sketch (0004_agents.sql — final shape at implementation)

- `agent_sessions`: id, run_id FK runs, role (5-value check), status queued|claimed|running|completed|failed|killed|budget_exhausted, worker_id, heartbeat_at, claimed_at, started_at, finished_at, error_note, steps_used int, cost_millicents bigint, step_ceiling int, cost_ceiling_millicents bigint, created_at.
- `session_steps`: id, session_id FK cascade, idx int, kind text (navigate|act|observe|judge|assert|error), decision jsonb, action jsonb, evidence jsonb (artifact refs), tokens_in int, tokens_out int, cost_millicents bigint, created_at. Append-only: no updates (enforced by no update path in code).
- `plan_resolution` already exists (0003) — sessions reference run_id like it.

## Verification approach

- Orchestrator: atomic-claim race test (two claimers, one winner), orphan sweep test (heartbeat lapse -> requeue), kill test (kill mid-flight -> agent exit, session killed).
- Agent loop: against a local fixture page (bet-app staging URL config + a static fixture page for unit tests): login flow drives to success; budget exhaustion stops mid-run with BudgetExhausted terminal status; kill switch stops; every step row cites evidence.
- Vision judge: fixture evidence with machine-checkable assertion pass -> verified; absent evidence -> inconclusive (unit).
- Gateway client: fake OpenAI-compatible server (httptest in TS) — request shape, usage parsing, 402/budget-error mapping.
- Report: trajectory renders from seeded steps; verdict without evidence -> INCONCLUSIVE.

**Research date:** 2026-09-20 · Open questions: none open — 10 closed above with fallbacks each.