# Pitfalls Research — v1.1 Admin Cockpit

**Domain:** Cockpit-grade manual controls over an autonomous QA pipeline (manual run/derive triggers, approval workflow, live job/run observability, budget discipline) layered onto the existing report-only e2ai platform
**Researched:** 2026-09-22
**Confidence:** HIGH on budget/dedupe/concurrency pitfalls (read directly from shipped code, quoted below), HIGH on approval-race mechanics (state machine read from code), MEDIUM on UI/permission-model pitfalls (patterns read, exact blast radius depends on BFF config), LOW where marked [INFERENCE]

**Scope note:** This file covers *cockpit* failure modes only. The v1.0 `PITFALLS.md` (git history) already catalogues agent-domain pitfalls — self-grading/hallucinated verification, runaway agent loops, failure misattribution, self-healing masking regressions, tautological tests, temp-0 non-determinism, prompt injection via app content, iOS/Android simulator flakiness, organizational trust collapse. None are repeated here; the "Amplification" table at the bottom maps where cockpit controls make a v1.0 pitfall worse.

**The single most important takeaway:** the budget engine's `0 = unlimited` convention plus the enqueue path's unset cost ceiling means a naively-wired cockpit trigger can mint **sessions with no cost bound at all** (`ceiling := len(plan.Steps) // 0 = unlimited per the budget engine` — `api/internal/agents/http.go`, Enqueue; the agent_sessions insert there sets only run_id, role, step_ceiling — never the cost ceiling column). Every cockpit trigger path must be designed budget-first, not budget-last.

---

## Critical Pitfalls

Mistakes that cause real money burned or the core safety invariant broken.

### P1. Ceiling `0` means unlimited — a trigger path can mint unbounded-budget sessions
**What goes wrong:** The budget engine treats zero ceilings as *no ceiling*: the AppendStep guards read `if stepCeiling > 0 && ...` and `if costCeiling > 0 && ...` (`api/internal/agents/budget.go`). The only enqueue path today sets `step_ceiling = len(plan.Steps)` and **never sets `cost_ceiling_millicents`** (`api/internal/agents/http.go`, Enqueue — the agent_sessions insert names run_id, role, step_ceiling only). So: an approved plan that parses to zero steps gives every session an **unlimited step ceiling**, and every session today has an **unset (unlimited) cost ceiling**. The per-model cost accumulator runs (`CostMillicents`, step insert) but nothing ever stops on it.
**Why it happens:** "0 = unlimited" is a deliberate sentinel (`agents/http.go` comment says so verbatim), but the sentinel converts a degenerate input (empty/steps-in-other-field plan) into unbounded spend instead of a rejection.
**Code paths:** `api/internal/agents/budget.go` (AppendStep guards), `api/internal/agents/http.go` (Enqueue insert), `api/internal/app/migrations/0004_agents.sql` (`step_ceiling`, `cost_ceiling_millicents` columns, 0 defaults).
**Consequences:** A malformed approved plan + one cockpit click = unbounded agent spend across the roster; exactly the v1.0 "runaway loops" pitfall but now *operator-invoked* and harder to blame on the agent.
**Prevention:** The cockpit trigger path must (a) reject plans that parse to zero steps before enqueueing, (b) always write an explicit `cost_ceiling_millicents` (from app config or a manual-trigger cap), (c) treat ceiling-0 as a validation error, not a sentinel, at the *trigger API boundary* — the engine's sentinel semantics stay, but no caller may pass 0.
**Detection:** Alert on zero-ceiling sessions at insert time; assert in tests that every manual-trigger enqueue carries both ceilings positive.

### P2. Budget enforcement is per-session only — nothing aggregates per run, per app, or per day
**What goes wrong:** The engine hard-stops inside the step-append transaction on `agent_sessions` columns alone (`budget.go` AppendStep: lock row, read that session's `steps_used`/`cost_millicents`, compare to that session's ceilings). A run spawns **two sessions per enqueue** (Navigator + VisionJudge — `agents/http.go` Enqueue loops `[]Role{RoleNavigator, RoleVisionJudge}`), and nothing sums them. Ten parallel runs on one app = twenty independent budgets, each individually "within ceiling".
**Why it happens:** BUDG-01/02 scoped the engine to the session as the unit of spend; v1 had no operator-initiated fan-out, so session-level ceilings were sufficient. The cockpit makes triggering cheap and tempting, which multiplies concurrent sessions.
**Code paths:** `api/internal/agents/budget.go` (whole engine), `api/internal/agents/http.go` (session fan-out), `api/internal/trends/` (cost per run *read-only*, not enforced), `api/internal/app/migrations/0004_agents.sql`.
**Consequences:** A "just re-run that suite a few times" habit in the cockpit produces cost multiples invisible until the trends dashboard catches up (post-hoc).
**Prevention:** v1.1 must add a run-level or app-level budget gate *at trigger time*: before creating a run/sessions, check aggregated `agent_sessions.cost_millicents` for the app over a window against a configured cap, and refuse (409 + surfaced reason) when breached. This is the concrete shape of "a UI trigger path must not bypass budget checks" — the bypass risk is real because the existing checks are session-scoped and trigger-time has no check at all today (`api/cmd/e2ai-api/main.go:405-414` derive-draft has no budget gate; Enqueue has none).
**Detection:** Trends alert on app-level cost velocity (0008 alerts infra exists — `api/internal/trends`, migration `0008_alerts.sql`); gate reads the same source of truth so the UI can pre-flight and show remaining budget before the operator clicks.

### P3. Enqueue's dedupe is read-then-insert — concurrent triggers race into duplicate paid sessions
**What goes wrong:** Enqueue guards double-enqueue with a count query (`select count(*) from agent_sessions where run_id = ...`) then inserts if the count is 0 (`api/internal/agents/http.go`, ~L191-199). Two concurrent POSTs (double-click, retry, two admins) both read 0 and both insert → **two Navigator + two VisionJudge sessions** on one run, each with its own budget. If 0004's per-run-per-role unique constraint exists, the loser instead surfaces as a raw 500 ("insert agent session failed") — an unhandled, confusing failure either way.
**Why it happens:** The TOCTOU pattern was accepted elsewhere with justification: the edge sweep's read-then-insert race costs "a wasted bounded session, not corruption" (`api/internal/agents/edgesweep.go` doc). That tolerance does not transfer to cost-incurring sessions.
**Code paths:** `api/internal/agents/http.go` (Enqueue), `api/internal/agents/edgesweep.go` (the tolerated analog), `api/internal/app/migrations/0004_agents.sql`.
**Consequences:** Duplicate spend (P1/P2 multiply it), confusing run detail with paired sessions, non-deterministic verdicts from competing navigators.
**Prevention:** Make the insert itself the gate: conflict-do-nothing on (run_id, role) mapping zero-rows-affected to 409 — or a River `UniqueOpts` job as `derive/job.go:40-47` already does. The cockpit trigger endpoint for *runs* must use the same pattern from day one.
**Detection:** Test with two parallel enqueue calls; assert exactly one 202 and one 409.

---

## High Pitfalls

Wrong results or broken invariants that survive review because they look like edge cases.

### P4. Approving a plan mid-flight flips the gate for runs the approver never saw
**What goes wrong:** Approval is *plan-level* state, not run-level: `approvals/machine.go` Decide sets the chosen plan_version to approved and **supersedes the previously approved version** ("approving version B supersedes A; A keeps existing for diffing, it just stops being current"). Meanwhile the run-side gate is `mode='approved' and plan_version.status='approved'` re-checked at spawn time (`agents/http.go` Enqueue; `agents/worker.go` checkPreconditions queries the plan row as "the cross-check for anything that mutated since"). Two race shapes:
1. A run pinned to version A (via `run_plan_resolutions`, content_hash recorded at derive) is still queued/provisioning when the approver flips the plan to version B → the spawn-time cross-check sees plan-level status approved (true) but the *version* it resolves is B, not A — if the resolution's version row is now superseded, behavior depends on whether the gate reads the pinned version's own status (superseded → spawn fails or, worse, passes if superseded still counts as approved).
2. The cockpit's "trigger run from plan" button reads the plan's *current approved* version; the approver swaps it between the click and the derive job resolving → the run executes a different plan than the operator previewed.
**Why it happens:** The state machine deliberately keeps one current approved version per plan (`approvals/machine.go` rank expression; `plans/sync.go` upsertPlanVersion), and run pinning snapshots *content* (content_hash) but the gate re-reads *status*.
**Code paths:** `api/internal/approvals/machine.go` (Decide, supersede), `api/internal/plans/sync.go` (upsertPlanVersion, plan status refresh), `api/internal/agents/worker.go` (checkPreconditions), `api/internal/agents/http.go` (Enqueue resolution join), `api/internal/app/migrations/0002_plans.sql` (plan_versions status vocabulary).
**Consequences:** Run executes a plan the operator didn't preview; reviewer intent (approve the diff they read) silently diverges from what runs; a superseded-pinned run either fails confusingly or runs stale content.
**Prevention:** Pin the *version id + content hash* at trigger time and have the cockpit confirm the hash right before enqueue (show what will run, resolve the exact version row); make the spawn gate accept the pinned version's recorded hash as authoritative when its own status moved to superseded *after* the run was created (record superseded_at, compare timestamps) — never re-resolve "current approved" at spawn for a run that already pinned.
**Detection:** 409 test: decide on plan B while a run pinned to A is queued → run must either start with A or fail with a typed "plan superseded post-pin" note, never silently swap. The UI must render the pinned content hash on run detail.

### P5. Manual-trigger run ids collide with the dedupe gate — by design it will be the *next* bug
**What goes wrong:** Run-level dedupe is `(app_id, workflow_run_id)` unique (`api/internal/ingest/dedupe.go`, insertRun on-conflict-nothing). Webhook runs carry GitHub's positive ids; sweeps dodge collision with **negated wall-clock milliseconds** (`agents/edgesweep.go`: "negated wall-clock millisecond — disjoint from positive GitHub ids"). A cockpit trigger must synthesize an id too, and naive choices fail: second-granularity timestamps collide under double-click (second unique → 500/409 confusion); `0` or a constant collides with itself forever; even ms-granularity collides if two triggers land in the same millisecond across processes. Worse, a *re-trigger of the same PR head* is a legitimate cockpit use case that the current unique key actively blocks (same workflow_run_id would be reused if naively derived from the workflow run).
**Why it happens:** The id space was designed for two producer families (GitHub webhook, sweep); cockpit adds a third with different cardinality needs.
**Code paths:** `api/internal/ingest/dedupe.go` (insertRun on-conflict), `api/internal/agents/edgesweep.go` (negated-ms convention), `api/internal/app/migrations/0001_init.sql` + `0007_hardening.sql` (trigger_source vocabulary: webhook/reconcile/sweep — 'manual' does not exist yet; the check constraint will *reject* it until migrated).
**Consequences:** Trigger button errors on second press; or worse, silent dedupe swallowing an intentional re-run (the insert returns "duplicate" and the cockpit shows nothing happened).
**Prevention:** Adopt the sweep convention for the new source: negated, monotonically finer id (e.g. negated micro/nanosecond + random suffix) or a dedicated sequence, **and** widen `runs_trigger_source_check` to include the new source in the same migration as the trigger feature (0007 precedent: the check constraint was added in 0007 when 'sweep' shipped).
**Detection:** Migration must fail closed: attempt a manual-source run before the constraint is widened → constraint violation proves the hole; add a regression test pairing the new trigger_source with the dedupe path.

### P6. Sticky PR comment is last-writer-wins — a manual run on the same SHA clobbers the deploy run's report
**What goes wrong:** The report publisher resolves the PR by head SHA, creates-then-patches a single sticky comment per (app, pr_number), and records `last_run_id` with a conflict-do-update upsert (`api/internal/report/comment.go` Publisher doc: "create once, then PATCH the same comment"; recordURLs upserts pr_comments setting comment_id/last_run_id). Two runs on the same head SHA (deploy webhook + cockpit-triggered re-run) **race to patch the same GitHub comment**: concurrent PATCHes are last-request-wins on GitHub's side, and the local pr_comments row's last_run_id flips to whichever finished last — so the PR's visible report can be the *manual* run's verdict while the deploy's report URL is lost from the run row.
**Why it happens:** One-comment-per-PR is the right UX for webhook cadence (one deploy = one report); the cockpit makes N runs per PR possible.
**Code paths:** `api/internal/report/comment.go` (Publisher, recordURLs ~L222-247), `api/internal/app/migrations/0001_init.sql` (pr_comments unique (app_id, pr_number)), `api/internal/ingest/dedupe.go` (per-app delivery dedupe — unrelated, does not help here).
**Consequences:** Deploy-run evidence overwritten by an exploratory manual run in the one place humans read it; `last_run_id` no longer points at the authoritative run; confusion about which verdict is "the" verdict for the PR.
**Prevention:** The cockpit's manual trigger must decide its reporting contract explicitly: either (a) manual runs post to a separate surface (run detail page only — report-only posture preserved), (b) manual runs append an ephemeral reply comment clearly labeled, or (c) sticky-comment publishing is gated to the deploy-sourced run for a given head SHA (check trigger_source before publishing). Do (c) + (a) as the floor: never let a manual run silently overwrite the deploy report.
**Detection:** Test: run A (webhook) and run B (manual) same SHA → B must not PATCH the sticky comment; pr_comments.last_run_id must still be A's run.

---

## Moderate Pitfalls

### M1. 202-then-silence: trigger feedback has no lifecycle to report
**What goes wrong:** Today's only mutation-ish endpoints return 202 and walk away: derive-draft enqueues a River job (`cmd/e2ai-api/main.go:405-414`) and Enqueue inserts sessions and returns counts (`agents/http.go`). River job state (available/running/retryable/discarded) is **not exposed to the UI at all** — the runs list derives everything from run/session rows. A cockpit "Trigger" button that gets 202 has nothing to show while the job sits queued: the run stays `queued` (or the derive draft never appears), the operator has no way to distinguish "slow worker" from "job failed and was not retried" (derive jobs are `MaxAttempts: 1`, `derive/job.go:40-47` — one failure and the job is dead, with the error only in river_job's error column, invisible to any cockpit surface).
**Why it happens:** Webhook cadence meant no human was watching the gap between delivery and run creation; sweeps are periodic and self-healing via the orphan sweep (`agents/sessions.go` OrphanSweep requeues lapsed sessions).
**Code paths:** `api/cmd/e2ai-api/main.go` (derive-draft handler), `api/internal/derive/job.go` (MaxAttempts 1, UniqueOpts), `api/internal/platform/river/river.go` (queue split: api drains "default", runner drains "runner"), `web/src/lib/usePolling.ts` + `web/src/features/runs/RunDetailPage.tsx` (5s polling, latest-payload-wins, stops on hidden).
**Consequences:** The cockpit's flagship feature (click → watch) degrades into a spinner of unknown duration; support burden; operators re-click, hitting P3.
**Prevention:** Expose job liveness at the trigger boundary: on trigger, create the run row *synchronously* in the same tx as the job enqueue (ingest precedent — `ingest/dedupe.go` commits run row + enqueue atomically), so the run row exists immediately with a status the UI can poll; add a "why is nothing happening" surface (last transition timestamp + job error if derivable). For derive-draft specifically, make the handler return the run id and let the detail page poll the resolution row's appearance.
**Detection:** Synthetic monitor: trigger → poll run row until resolution or N minutes → assert a terminal or in-flight state, never "queued with no transitions and no session rows for > X min".

### M2. Cancellation races: kill switch semantics are typed but the cockpit must not re-invent them
**What goes wrong:** The kill switch and finalize path already coordinate through typed rejections: "A rejection is expected when someone else finalized first (kill switch 409, budget engine breach): the row's own status is authoritative" (`agents/worker.go` finish doc), and heartbeat's terminal-session rejection is where kill-switch 409s land (`agents/sessions.go` Heartbeat doc). The cockpit adds operator-facing cancel — the failure modes are (a) calling the underlying transition directly instead of the guarded session Transition (bypassing 409 semantics), (b) killing a run *row* without killing its live sessions (orphan sweep then "rescues" a session the operator killed, resurrecting spend), (c) treating the 409 as an error toast instead of "someone finalized first" (UI lies).
**Code paths:** `api/internal/agents/sessions.go` (Transition guard, Heartbeat terminal rejection, OrphanSweep), `api/internal/agents/worker.go` (finish, 409 tolerance), `api/internal/runs/state.go` (run Transition rejects forbidden transitions + concurrent drift).
**Prevention:** Cockpit cancel must be one operation over the run+sessions aggregate: for each non-terminal session issue the kill (tolerate 409 as success), then transition the run to failed with a note; never raw row updates. Idempotent re-cancel must be a no-op, not an error.
**Detection:** Test: cancel a run whose navigator session just completed → cancel returns success, vision-judge session is killed, run terminal; second cancel is a no-op.

### M3. Trigger capacity: the trigger path shares the default queue with pipeline plumbing
**What goes wrong:** The api process drains only the "default" queue; the runner process drains only "runner" (`platform/river/river.go` — "mirror of the incidents-admin jobs factory"). The default queue carries ingest-derived jobs, derive, reconcile sweep, orphan sweep, and (future) cockpit-triggered work. [INFERENCE on exact worker counts — capacity config lives in `cmd/e2ai-api` client construction.] A cockpit that lets N admins trigger M runs at once contends with the pipeline's own housekeeping jobs on the same small pool; conversely a pipeline backlog (many webhook events after a deploy) delays the operator's manual derive behind it, feeding M1's "202-then-silence".
**Prevention:** Put cockpit-triggered jobs on a dedicated queue (River queue names are cheap) so operator-visible latency is independent of pipeline load; keep MaxAttempts discipline per job kind explicit rather than inherited.
**Detection:** Load test: 50 webhook events + 1 manual trigger → manual derive latency measured and documented.

### M4. Audit trail gaps: the cockpit's every action must be attributable, and nothing today records an actor for runs
**What goes wrong:** Approvals are attributed (approver, notes — `approvals/machine.go` Decide, APPR-02), but runs carry only `trigger_source`; agent_sessions have no actor; kill switches have no operator record; quarantine activation has no actor (0007 `test_quarantine` has activated_by? — [INFERENCE: verify in 0007]). The cockpit makes mutations *human-initiated*, so "who did this" becomes a first-class question the schema cannot answer today.
**Code paths:** `api/internal/app/migrations/0001_init.sql` (runs.trigger_source, no actor), `0007_hardening.sql` (quarantine), `api/internal/flakes/` (activate path), `api/internal/platform/httpx/auth.go` (X-User available server-side — `UserOf`).
**Prevention:** One audit table (actor, action, target type/id, timestamp, notes) written in the same tx as every cockpit mutation — the ingest tx pattern (`ingest/dedupe.go`: delivery + run + enqueue atomic) is the template. Backfill actor onto new columns for trigger_source='manual' runs at creation.
**Detection:** Review gate: any new cockpit endpoint must show its audit write in the diff; test asserts the row exists after the mutation.

### M5. Permission ladder is two rungs and handlers hand-roll the checks
**What goes wrong:** Today's vocabulary is RunViewPermission / RunEditPermission (+ defaults map in `httpx/auth.go`), and every handler re-implements the same switch (Enqueue: `requirePermission(... RunEditPermission)`; derive-draft in main.go: identity-from-context + HasPermission + 403; trends: a *conditional* ladder where `need` is chosen per request; report/page.go: view-tier fallback logic at L501-505). The cockpit asks "who may trigger?" — a question the two-rung ladder cannot answer distinctly from "who may kill a session" or "who may approve a plan", which are materially different authority levels currently sharing RunEdit.
**Code paths:** `api/internal/platform/httpx/auth.go` (consts, HasPermission, DevFallbackUser defaults), `api/internal/agents/http.go`, `api/cmd/e2ai-api/main.go`, `api/internal/trends/http.go`, `api/internal/report/page.go`.
**Consequences:** Either the ladder grows per-endpoint checks with drift (one handler forgets the gate → a trigger path with *no* permission check, the exact budget-bypass-shaped bug), or everything stays under RunEdit and any editor can spend money.
**Prevention:** Introduce a trigger/admin split in the permission vocabulary *before* cockpit endpoints land (e.g. RunTrigger, PlanApprove, RunAdmin), and extract one `require(w, r, perm)` helper used by every cockpit handler; dev-fallback identity must NOT default-grant the new permissions (it currently grants the full defaults map — check `DevFallbackUser`).
**Detection:** Table-driven permission tests over the route map: each route × (no identity, view-only, editor, admin) → expected code; one source of truth for the map.

### M6. BFF header trust: identity is reconstructed from headers, so the cockpit's security floor is the BFF's
**What goes wrong:** `httpx` middleware trusts `X-User`/`X-Permissions` headers injected by the admin BFF (`httpx/auth.go` doc: "the admin-bff terminates the user session and forwards X-User/X-Permissions"), with a dev fallback ("you" for un-proxied traffic) and a bearer path (`bearerUser`). Any route to e2ai-api that bypasses the BFF (direct pod access in cluster, port-forward in dev, a misconfigured gateway route for the `/e2ai` prefix — see `web/src/shared/gateway.ts`: BFF strips the `/e2ai` prefix) receives no headers → dev fallback kicks in → possibly an identity with full default permissions.
**Prevention:** The cockpit must not weaken this: e2ai-api must reject mutation requests that arrive with *no* identity headers in non-dev mode (today the fallback yields "you" silently — check whether HasPermission still true for DevFallbackUser); document the boundary (BFF is the authN point; api is the authZ point; nothing else may reach it) and enforce at the ingress (NetworkPolicy/service mesh) not just convention.
**Detection:** Security test: mutation request without headers against a prod-config instance → 401/403, never success. Verify gateway routes for `/e2ai/api/*` terminate at the BFF.

### M7. i18n: every cockpit string lands in four locales or the surface ships half-translated
**What goes wrong:** The module carries en-US, es, pt-BR, ro locale files (`web/src/i18n/locales/`) and the shared admin shell renders via `@nsxbet/admin-sdk` useI18n; a cockpit with action buttons, confirmation dialogs, budget warnings, and error toasts will add dozens of keys — a missing key shows the raw key or falls back (module convention), which in a *control surface* reads as broken rather than untranslated.
**Prevention:** Add the key-lint to the module's existing manifest validation (`web/scripts/validate-manifest.ts` precedent) or a CI check that all four locale files share the same key set before cockpit pages merge.
**Detection:** Build check comparing locale key sets; failure blocks merge.

---

## Minor Pitfalls

### m1. Locale keys (see M7) — fold the check into manifest validation rather than a new lint pipeline.

### m2. Trigger endpoint under the existing route style (`cmd/e2ai-api/main.go` ServeMux "POST /api/runs/...") is fine, but cockpit pages must route through the BFF (`web/src/shared/gateway.ts` `${bffBaseUrl}/e2ai/api/*`) — a fetch that targets the module origin's `/api/` prefix (admin-api's, not e2ai's) silently hits the wrong backend. Prevention: one `e2aiApi` client path constructor (`web/src/lib/api.ts` already centralizes — extend, don't parallel).

### m3. The `usePolling` hook stops when the tab is hidden and resumes on visible (`web/src/lib/usePolling.ts`) — fine for reports, wrong for a trigger confirmation dialog: an operator clicking Trigger then tabbing away and back will see a *stale* pre-trigger payload for up to one interval, believe the trigger failed, and re-click. Prevention: mutation-triggered immediate refetch (call the loader directly after POST returns), not interval-bound patience.

---

## Amplification: cockpit controls × v1.0 agent-domain pitfalls

| v1.0 pitfall (git history, 2026-09-19) | How the cockpit amplifies it | Guard |
|---|---|---|
| Runaway agent loops / cost blowups | One-click triggers + no aggregate budget (P1/P2) turns a rare accident into a cheap habit | Trigger-time aggregate budget gate; explicit ceilings mandatory |
| Failure misattribution (agent vs product vs infra) | Manual runs mix into webhook runs on the same PR; sticky comment race (P6) obscures which verdict belongs to which run | trigger_source + actor audit; reporting contract per source (P6) |
| Self-grading / hallucinated verification | Cockpit verdict display invites treating the UI's green as truth at a glance | Show evidence links + verifier independence markers on the run page (v1.0 checklist carries over) |
| Organizational trust collapse | A broken trigger UX (202-then-silence, M1) or a clobbered PR report (P6) burns trust in the *platform*, faster than agent flakiness ever did | Trigger lifecycle visibility; manual runs never overwrite deploy reports |
| Simulator/emulator flakiness | "Retry" buttons invite retry storms → cost (P2) and flake-statistic distortion (trends read all runs equally) | Retry = new run with its own budget; consider excluding manual re-runs from flake-rate denominators or tagging them |
| Prompt injection via app content | Cockpit pages render plan titles/PR titles/agent notes from app-controlled content | React escaping holds; do not add dangerouslySetInnerHTML or rich rendering of plan content without sanitization |

---

## Phase-Specific Warnings

| Phase topic (likely v1.1 phase) | Likely pitfall | Mitigation (from above) |
|---|---|---|
| Manual trigger API | Run-id synthesis colliding with dedupe key; constraint rejects new trigger_source | P5: dedicated id space + widen check constraint in same migration |
| Trigger API | Sessions minted without cost ceiling; zero-step plan accepted | P1/P2: validate ceilings + aggregate budget at trigger time |
| Approval workflow UX | Approve-while-running races; double-decide on a superseded version | P4: hash-confirm + typed 409 (the approvals UI already handles 409 inline — `web/src/features/plans/ApprovalsQueuePage.tsx` doc) |
| Cockpit run/ops page | 202-then-silence; stale polling UX; cancel races | M1/M2/m3: synchronous run row, guarded cancel, post-mutation refetch |
| Permissions/audit | Two-rung ladder drift; unattributed mutations; BFF bypass | M4/M5/M6: new permission rungs, one require helper, audit table, no-header mutation rejection |
| Budget surfacing | Budget shown from a different source of truth than the engine | Read the same `agent_sessions` aggregates the gate will enforce (P2) |

---

## "Looks Done But Isn't" Checklist — Cockpit Edition

Before declaring any cockpit phase done, verify:

- [ ] Double-click the trigger → exactly one run, one 409 (concurrency test, not vibes)
- [ ] A zero-step approved plan is rejected at trigger time, never enqueued
- [ ] Every session minted by the cockpit has both ceilings strictly positive
- [ ] Approving B while a run is pinned to A: the run starts with A or fails typed — never silently swaps
- [ ] A manual run never PATCHes the sticky PR comment of a deploy run on the same SHA
- [ ] Interrupting a provisioning run: in-flight sessions end cleanly, the run lands in a terminal status, and a second press is accepted
- [ ] Budget-gate refusals surface in the UI with the reason (not a bare 500)
- [ ] Every cockpit mutation writes an audit row with the actor from the BFF headers
- [ ] Permission matrix test over the whole route map (no identity / view / edit / admin)
- [ ] All new strings present in en-US, es, pt-BR, ro
- [ ] A request lacking the BFF identity headers is refused in production wiring; the dev fallback identity never grants trigger rights
- [ ] Report-only invariant intact: no cockpit endpoint mutates the app under test (grep the diff for calls outside the e2ai DB/runner boundary)

## Sources

- Code-grounded (HIGH confidence by construction — every pitfall cites a path in this repo, read 2026-09-22):
  - api/internal/ingest/dedupe.go, ingest/http.go (webhook tx, per-app delivery dedupe, triage store, size limit)
  - api/internal/agents/edgesweep.go (negated wall-clock synthetic run ids, stateless tick, due-ness in SQL), agents/edgesweep.go:87-89 (sweep trigger_source filter)
  - api/internal/agents/worker.go (spawn-gate double-check of plan-level status), agents/http.go (Enqueue approved-plan gate, kill switch 409, ceilings from plan length)
  - api/internal/agents/budget.go (both ceilings skip enforcement when zero; step-append-tx enforcement), agents/sessions.go (Transition guard, orphan sweep 90s lapse, heartbeat rejection on terminal)
  - api/internal/agents/claim.go (atomic session claim), agents/repair.go (FLAK-02 intake)
  - api/internal/runs/state.go (Transition guarded UPDATE), runs/queries.go (read surface)
  - api/internal/suite/execute.go (resolvePlanGate upsert of run_plan_resolutions; content-hash pinning)
  - api/internal/approvals/machine.go (approval state machine, rank-based current-version swap, Decide attribution)
  - api/internal/derive/job.go (DeriveOpts UniqueOpts by-state; MaxAttempts 1)
  - api/internal/platform/river/river.go (default/runner queue split), api/cmd/e2ai-api/main.go (worker wiring, derive-draft endpoint, RunEditPermission checks)
  - api/internal/platform/httpx/auth.go (X-User/X-Permissions identity, DevFallbackUser, permission consts)
  - api/internal/report/comment.go (sticky-comment PATCH by marker scan; pr_comments last-writer-wins upsert)
  - api/internal/app/migrations/0001_init.sql (runs unique (app_id, workflow_run_id), pr_comments unique (app_id, pr_number)), 0002_plans.sql (plan_versions status check, plans current_version_id), 0007_hardening.sql (trigger_source check constraint now real, quarantine upsert guard, per-app sweep columns)
  - web/src/lib/usePolling.ts (latest-wins polling, hidden-tab pause), web/src/lib/api.ts (fetch client, typed errors), web/src/shared/gateway.ts (BFF base resolution), web/src/app/routes.tsx (module route map)
  - web/src/features/plans/ApprovalsQueuePage.tsx (decide flow, 409-superseded surfacing), web/src/features/runs/RunDetailPage.tsx (read-only polling detail)
- v1.0 lessons carried forward: .planning/research/PITFALLS.md (2026-09-19, git history preserves it) — self-grading, runaway loops, misattribution, self-healing masking, tautologies, non-determinism, prompt injection, simulator flakiness, trust collapse.

The cockpit-specific deltas are all grounded in today's code read; no external web sources were needed for this pass. Where the v1.0 file's lesson is amplified rather than restated, the amplification table above points at the concrete mechanism.
