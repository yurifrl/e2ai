# Project Roadmap: E2AI — the Always-On AI QA Arm

**Generated:** 2026-09-19 (v1.0) — v1.1 milestone added 2026-09-22
**Project:** @admin/e2ai (module `@admin/e2ai`, routeBase `/e2ai`)
**Requirements:** .planning/REQUIREMENTS.md — 72 total: 45 v1.0 (shipped) + 27 v1.1 Admin Cockpit, 100% phase-mapped
**Invariant:** report-only everywhere — nothing in this roadmap gates a merge, deploy, or CI check

## Phases

**Phase Numbering:**

- Integer phases (1, 2, 3): planned milestone work — continuous across milestones (v1.0: Phases 1–5 shipped; v1.1: Phases 6–9)
- Decimal phases (2.1, 2.2): urgent insertions via `/gsd-phase --insert`

- [x] **Phase 1: Web happy path: deterministic foundation** - webhook → deterministic suite → verdicts → live reports on every deploy
- [x] **Phase 2: Plans, approvals, and test derivation** - declarative plans + approval queue, PR-diff derivation, corpus oracle
- [x] **Phase 3: Agent roster and the agentic loop** - five governed agents drive web runs with budgets and a zero-credential sandbox
- [x] **Phase 4: Mobile surfaces** - Android emulator + iOS simulator, WebView-aware capture, pinned runner images
- [x] **Phase 5: Flake governance, trends, and multi-app scale** - flake governance, trends, MCP surface, edge sweeps, second app
- [ ] **Phase 6: Cockpit trigger, Command Center, and webhook simulate** - manual trigger behind a pre-flight budget gate, module-root Command Center, server-side simulate; mints the shared substrate (migration 0010, budget gate, audit, permissions)
- [ ] **Phase 7: Webhook Inbox** - deliveries inbox with ingest outcomes, raw payload viewer, GitHub redelivery, unclaimed-triage registration deep links
- [ ] **Phase 8: Outside-action consolidation** - kill, add-session, plans-sync, canary run-now, MCP probe, orphan sweep under one action contract
- [ ] **Phase 9: Apps registry completion and Jobs Observatory** - registry CRUD with vault-ref secrets + first admin-rung consumers, River jobs observatory with guarded cancel and queue pause

## Milestone Progress

| Phase | Milestone | Plans Complete | Status | Completed |
|-------|-----------|----------------|--------|-----------|
| 1. Web happy path | v1.0 | — | Complete | 2026-09-22 |
| 2. Plans, approvals, and test derivation | v1.0 | — | Complete | 2026-09-22 |
| 3. Agent roster and the agentic loop | v1.0 | — | Complete | 2026-09-22 |
| 4. Mobile surfaces | v1.0 | — | Complete | 2026-09-22 |
| 5. Flake governance, trends, and multi-app scale | v1.0 | — | Complete | 2026-09-22 |
| 6. Cockpit trigger, Command Center, and webhook simulate | v1.1 | 1/5 | In Progress|  |
| 7. Webhook Inbox | v1.1 | — | Not started | - |
| 8. Outside-action consolidation | v1.1 | — | Not started | - |
| 9. Apps registry completion and Jobs Observatory | v1.1 | — | Not started | - |

## Milestone v1.0 — the Always-On QA Arm (shipped 2026-09-22)

### Phase 1: Web happy path: deterministic foundation

**Goal:** The full report-only QA loop for bet-app web on every deploy: webhook → run → deterministic Playwright suite → verdicts → live HTML report + sticky PR comment.

**Delivers:**

- Admin-shell module skeleton (INTG-01..03): `admin.module.json` registration, Go hexagonal API + Postgres + River via Tilt with mock auth, report page with live run state
- Deploy ingest (INGE-01..03): HMAC-validated idempotent `workflow_run` ingest (202-then-enqueue), reconciliation sweep for missed deliveries, rollout-health gating before QA starts
- Deterministic suite (SUIT-01..02): existing bet-client Playwright specs run on staging per deploy; blob/json + traces consumed as evidence; baseline for what agents must later beat
- Verdict engine + reports (VERD-01, REPT-01..03, SECR-01): fixed taxonomy `PASS/FAIL/FLAKY/INFRA_ERROR/INCONCLUSIVE`, annotated-screenshot live reports, failure classification, sticky PR comment + neutral check, secrets hygiene
- Web surface (PLAT-01): viewport projects matching bet-client config
- App registry (APPS-01..02): config-as-code registration, bet-app as first app

**Requirements covered:** INTG-01..03, APPS-01..02, INGE-01..03, SUIT-01..02, VERD-01, REPT-01..03, SECR-01, PLAT-01 (16/45)

**Success criteria:**

- Push to staging → webhook lands → deterministic run executes → report page shows screenshots + explanations → PR comment sticks and updates
- Replayed duplicate webhook delivery produces exactly one run (idempotency)
- QA starts only after rollout health confirms the build is live
- Every verdict traceable to evidence; no exit-code-only verdicts

**Depends on:** bet-client deploy workflow + staging environment (external, exists today)
**Guardrails:** No agents yet — Phase 1 is 100% deterministic. No mobile. No plans/approvals yet.

### Phase 2: Plans, approvals, and test derivation

**Goal:** Human-supervised autonomy begins: declarative test plans with an approval queue, PR-diff-driven test selection and plan drafting, and the behavior-corpus oracle seeded from existing specs.

**Delivers:**

- Plan format + sourcing (PLAN-01..02): declarative markdown with frontmatter (sketch-006 contract), per-app registry sourcing (app repo path or central plans repo)
- Approval queue (APPR-01..02): new/changed plans wait for human approval; approved plans run unattended on deploys; rendered-plan + raw-diff approval UI; attributable approvals
- Test derivation (DERV-01..03): PR diff → affected scope → run plan; Plan Author drafts intent-level checks queued for approval; tautology guard (expected values from independent sources only)
- Corpus seed (CORP-01..02): expected-behavior store seeded from Playwright specs; agent observations captured as unverified learnings with human promotion

**Requirements covered:** PLAN-01..02, APPR-01..02, DERV-01..03, CORP-01..02 (9/45)

**Success criteria:**

- A plan change triggers the approval queue; approved version runs unattended on next deploy
- A PR touching known scope gets a right-sized run plan (subset, not full suite)
- A drafted plan referencing only the diff's own implementation is rejected by the tautology guard
- Corpus entries carry provenance; no entry self-mutates without human promotion

**Depends on:** Phase 1 (registry, runs, verdicts exist)
**Guardrails:** Still deterministic + plans only — no agentic execution yet. Derivation drafts, never auto-runs.

### Phase 3: Agent roster and the agentic loop

**Goal:** The product's core claim goes live: five governed agents drive the web loop — Navigator clicks, Vision Judge verifies — with independent context, hard budgets, and a zero-credential sandbox.

**Delivers:**

- Roster + orchestrator (AGNT-01..02): Navigator, Vision Judge, Plan Author, Edge-case Hunter, Flakiness Analyst with governed handoffs; River-queue session orchestration with heartbeat/wake, append-only run log, atomic claiming, orphan recovery
- Navigator (AGNT-03): a11y-tree-grounded locator-first navigation (Playwright + Stagehand); vision verifies, never clicks blind
- Vision Judge (AGNT-04): separate agent, independent context (different model family for high-stakes verdicts); oracle = expected-behavior store; deterministic side-effect assertions alongside VLM judgment
- Evidence-grade agentic reports (REPT-04): trajectory + per-step citations; no machine-checkable evidence → `INCONCLUSIVE`
- Budgets & safety (BUDG-01..02, SAFE-01): step ceiling, per-run cost accumulator with non-retryable `BudgetExhausted`, gateway-level budget keys; operator kill switch + stuck detection; zero-credential egress-allowlisted staging-only sandbox; full trajectory logging
- aihub gateway (AGNT-05): all model traffic through company gateway, no provider keys in agents

**Requirements covered:** AGNT-01..05, BUDG-01..02, SAFE-01, REPT-04 (9/45)

**Success criteria:**

- An agentic run drives bet-app login + one payment flow on staging and produces an evidence-cited report alongside the deterministic run
- Vision Judge verdicts that lack machine-checkable evidence render `INCONCLUSIVE`, never confident guesswork
- A runaway run hits `BudgetExhausted` and stops — non-retryable, report says so
- Sandbox: no credentials reachable from agent context; egress beyond allowlist blocked and logged
- Kill switch stops a mid-flight run without any deploy involved

**Depends on:** Phase 1 (runs, verdicts, reports) + Phase 2 (corpus oracle, approved plans)
**Guardrails:** Web only. Agents execute approved plans + bounded hunts; never merge-gate; never touch production.

### Phase 4: Mobile surfaces

**Goal:** The same loop reaches the Android emulator and iOS simulator: Maestro-first Android, simctl+idb-grounded iOS, WebView-aware capture, pinned runner images.

**Delivers:**

- Android (PLAT-02): Maestro YAML flows with native screenshots/video
- iOS (PLAT-03): `simctl` lifecycle + idb grounding primitives, Maestro as input/compat layer
- WebView-aware loops (PLAT-04): readiness probes, animation/network/render-idle before capture, double-lag handling; boot/screenshot failures auto-classified `INFRA_FLAKE`
- Runner discipline (PLAT-05): version-pinned Xcode/emulator images, deliberate upgrades behind canary runs

**Requirements covered:** PLAT-02..05 (4/45)

**Success criteria:**

- An approved plan with mobile surfaces executes on emulator + simulator per deploy; reports carry native screenshots/video
- A webview screen mid-animation captures only after readiness probes pass (no double-lag screenshots in reports)
- Emulator cold-boot failure lands as `INFRA_FLAKE` with the boot log attached, not `FAIL`
- Runner image upgrade can be canaried on one run before fleet-wide rollout

**Depends on:** Phase 3 (agentic loop + Navigator surface abstraction extends to mobile)
**Guardrails:** Real devices (TestFlight/cloud device farms) out of scope for v1.

### Phase 5: Flake governance, trends, and multi-app scale

**Goal:** Make it trustworthy at scale: flake rates and evidence-gated retries, refuse-to-heal selector repair, trends + cost dashboards, MCP surface for coding agents, bounded edge sweeps, multi-app onboarding.

**Delivers:**

- Flakiness governance (FLAK-01..02): per-test/app flake rates from run history, evidence-gated retries (never blind re-runs of `APP_BUG`), quarantine; stale detection + selector-repair proposals as reviewable diffs behind the refuse-to-heal gate
- Trends (TREN-01): verdict-flip rate as alerting metric, run history, cost per run/agent
- MCP surface (MCP-01): coding agents query runs, failures, evidence from a PR session
- Bounded exploration (EDGE-01): Edge-case Hunter scheduled sweeps, opt-in per app; unbounded exploration stays an anti-feature
- Corpus health (CORP-03): coverage of critical paths, stale behaviors, promotion queue
- Multi-app (APPS-03): new app = registry row + config

**Requirements covered:** FLAK-01..02, TREN-01, MCP-01, EDGE-01, CORP-03, APPS-03 (7/45)

**Success criteria:**

- A test with >20% flake rate across recent runs is flagged and can be quarantined from the report denominator
- A broken selector produces a reviewable repair diff only above the confidence floor — never auto-applied, never on error pages
- Verdict-flip spike surfaces on the trends dashboard before anyone asks
- A second app registers via config only and gets its first deterministic run end-to-end
- A coding agent in a PR session queries e2ai via MCP and links to evidence

**Depends on:** Phase 3 (agent roster) + accumulated run history from Phases 1–3
**Guardrails:** No auto-healing below the confidence floor. Edge sweeps opt-in, bounded.

## Milestone v1.1 — Admin Cockpit

**Milestone goal:** Admins run and steer the shipped report-only arm from the cockpit: trigger runs without a deploy, watch everything from a Command Center landing page, debug webhooks in an inbox, consolidate operator actions, and manage the registry and River jobs. Zero new dependencies — a new web feature section plus a new Go surface package built strictly from patterns already in this repo (RTK/Connect-RPC stay out per the stack-reality decision in PROJECT.md).

**Order is deliberate (research MVP consensus):** Phase 6 mints the shared substrate every later phase inherits — migration `0010_cockpit.sql`, the trigger-time budget gate (pitfalls P1–P3 prevented at the trigger boundary), the audit-attribution pattern (M4), and permission wiring. The Webhook Inbox is maximal pain-per-effort; Outside-Action Consolidation is pure wiring; Registry/Jobs is the largest new API surface and carries the open design decisions flagged below. v1.0 pages stay additive-only throughout: new actions attach to existing pages, nothing is reworked.

### Phase 6: Cockpit trigger, Command Center, and webhook simulate

**Goal:** Operators run and steer the arm from a single pane: manual triggering behind a visible pre-flight budget gate, a Command Center as the module's landing page, and server-side webhook simulation — the phase that mints the shared substrate (migration 0010, budget gate, audit attribution, permission wiring) every later phase reuses.

**Delivers:**

- Shared substrate: `0010_cockpit.sql` (widens the `trigger_source` check constraint for `'manual'` + audit attribution columns); trigger-time budget gate (BUDG-03: strict-positive ceiling validation — ceiling 0 is rejected at the trigger API boundary, aggregate app/day spend against caps, insert-serialized concurrent triggers on the racy read-then-insert path); M4 audit row written in the same tx as every cockpit mutation (ingest tx as template); `admin.e2ai.edit` permission wiring with BFF identity attribution
- Manual trigger (TRIG-01..05): trigger from cockpit/registry card/plan page/re-run stamped with operator identity + optional reason, bypassing wait-rollout; approved-versions-only picker with disabled drafts and a zero-approved-apps escape hatch to Approvals; confirm showing resolved plan identity, current budget state, and run shape; append-only re-run semantics ("same commit (reproduce)" pins the original head SHA, "latest head (retest)" pulls current default branch); synthetic run ids from a dedicated sequence with a fail-closed uniqueness regression test; pinned plan version id + content hash at trigger time with a spawn gate that accepts the pinned hash even if superseded after run creation
- Command Center (COCK-01..04): sketch 001-A as the module-root landing page — KPI header (runs today, pass rate today, pending-approvals badge, budget spend vs cap, paused/kill-switch state), reverse-chron live feed with inline detail strips + app filter chips, agent rail with API-computed stuck flags (>90s lapsed heartbeat) linking to Run Detail, global pause control in the KPI header (paused blocks all new triggers with an explicit reason; state visible at all times)
- Webhook simulate (ACTN-01): pick app + payload shape; the api server renders and HMAC-signs with the app's resolved secret (secrets never reach the browser); result shows ingest outcome + created run link — trigger and simulate both drive the Ingest seam
- Comment discipline: sticky PR comments publish only for deploy runs (`trigger_source`-gated); manual runs report on Run Detail only, never racing the deploy run's sticky comment

**Requirements covered:** COCK-01..04, TRIG-01..05, ACTN-01, AUDT-01, BUDG-03 (12/27)

**Success criteria:**

- An admin opening the module root lands on the Command Center: the KPI header shows runs today, pass rate today, pending-approvals badge, budget spend vs cap, and pause/kill state; the reverse-chron feed updates as runs progress, rows expand inline to verdict/duration/cost/evidence, and app filter chips constrain the feed
- The agent rail lists live sessions (role, status, heartbeat age) and flags stuck sessions (>90s lapsed heartbeat) with a link to Run Detail; session failures, quarantined flakes, and triage arrivals appear as feed events
- An admin triggers a run without a deploy from cockpit, registry card, plan page, or run re-run: the run carries `trigger_source='manual'`, operator identity, and optional reason, and starts without wait-rollout; re-run always appends a new run row (reproduce pins the original SHA, retest pulls latest head) and manual synthetic ids never collide with the `(app_id, workflow_run_id)` webhook-dedupe key
- The trigger picker lists only approved plan versions (drafts disabled with a one-line reason; zero-approved apps link to their Approvals-queue plans); triggering is disabled with an explicit reason when budget is exhausted or the arm is paused — never silently queued; two concurrent triggers serialize to exactly one 202 and one 409; a zero-ceiling plan is rejected at the trigger boundary
- Simulate webhook produces an ingest outcome and, when accepted, a created-run link, with secrets rendered and HMAC-signed server-side only; every cockpit mutation leaves operator identity + timestamp answerable from the affected row in the UI; a manual run on the same SHA as a deploy run never PATCHes the deploy run's sticky PR comment

**Depends on:** v1.0 (deploy ingest seam, runs, verdicts, reports, approvals — the cockpit actuates the shipped pipeline; no v1.1 phase can precede it)
**Guardrails:** Report-only invariant — trigger/simulate/pause actuate the arm, never the app under test. Ceiling-0 rejected at the trigger boundary (engine sentinel semantics unchanged). Spawn gates accept the pinned hash; no re-resolving "current approved" for pinned runs. No auto-approval — the Approvals queue stays the only approval path.

**Plans**: 1/5 plans executed (waves 1→5, serial — shared files and trigger-service reuse)
**Wave 1**

- [x] 06-01-PLAN.md — Shared substrate: migration 0010 (trigger_source widen, audit_events, cockpit_state, budget cap, runs.triggered_by, manual-id sequence), audit writer, pause state + KPI pause endpoint, budget gate domain

**Wave 2** *(blocked on Wave 1 completion)*

- [ ] 06-02-PLAN.md — Manual trigger backend: atomic trigger tx (gates → insert → plan pin → audit → enqueue), approved-only picker data, pre-flight budget + plan identity, reproduce/retest semantics, spawn-gate pin acceptance, sticky-comment trigger_source gate

**Wave 3** *(blocked on Wave 2 completion)*

- [ ] 06-03-PLAN.md — Webhook simulate + feed/KPIs backend: server-side render + HMAC sign against own ingest seam, ingest outcome + created-run link, live feed + KPI aggregates + agent rail with stuck flags

**Wave 4** *(blocked on Wave 3 completion)*

- [ ] 06-04-PLAN.md — Command Center web read surface: route + CommandCenterPage, KpiCards, live feed panel, agent rail (view-only, polling)

**Wave 5** *(blocked on Wave 4 completion)*

- [ ] 06-05-PLAN.md — Action surfaces + cutover: TriggerPanel, SimulateCard, route swap to Command Center root, dev-auth editor path fix, i18n four locales, re-run actions on Run Detail

**UI hint**: yes

### Phase 7: Webhook Inbox

**Goal:** Webhook debugging leaves psql/curl: every delivery lands in a cockpit inbox with its ingest outcome, raw payload, and recovery actions, and orphaned ("belongs to no registered app") deliveries become first-class rows that resolve by registering the app.

**Delivers:**

- Deliveries page (INBX-01): reverse-chron webhook events with app badge, signature badge, and ingest-outcome chips (ingested→run link, duplicate, redelivered, triaged, rejected); row expand shows parsed metadata, raw JSON payload viewer, and links (View run, Open in GitHub)
- Redeliver (INBX-02): per-delivery GitHub redeliver action with a consequence sentence and cooldown; re-ingest flows the same idempotent dedupe path (duplicate = harmless, recorded)
- Unclaimed triage as first-class inbox rows (INBX-03): "belongs to no registered app" rows with a register-app deep link pre-filling owner/repo/workflow from the payload; on save the triage row resolves and past unclaimed deliveries for that repo show "now claimed" (no retroactive re-ingest)
- Mounted endpoints: `GET /api/cockpit/webhook-events` + `/api/cockpit/triage` (mounts the existing `ListTriage` query) + ingest re-drive retry — routed via the BFF `e2aiApiBase`, never the module origin's `/api/`
- Cross-phase contract: the register-app deep link targets the registration surface; where that surface is Phase 9's wizard (APPS-05), the wizard honors the pre-fill contract established here

**Requirements covered:** INBX-01..03 (3/27)

**Success criteria:**

- Today's webhook deliveries render reverse-chron with app/signature badges and outcome chips; expanding a rejected or triaged row shows the parsed metadata and raw payload that explain the outcome, with working View run / Open in GitHub links where applicable
- Redelivering a delivery re-flows GitHub's event through the same dedupe path: a duplicate lands harmless and recorded — never a second run for the same run id
- An unclaimed triage row states it belongs to no registered app; its register-app deep link carries owner/repo/workflow pre-filled from the payload; after a save the row resolves and that repo's past unclaimed deliveries flip to "now claimed" with no retroactive re-ingest
- App and outcome filters constrain the inbox and polling keeps it live without a page reload

**Depends on:** Phase 6 (audit + permission substrate, BFF route pattern; the re-drive retry inherits the audit pattern)
**Guardrails:** Read surface + bounded retry actions only. Redeliver is the sole external call (GitHub API), behind a consequence sentence + cooldown. No retroactive re-ingest of claimed deliveries.
**Plans**: TBD
**UI hint**: yes

### Phase 8: Outside-action consolidation

**Goal:** The remaining operator actions reach the cockpit under one consistent action contract — kill with reason, mid-run session add, one-click plans-sync, canary run-now, MCP probe, orphan sweep — attached to existing v1.0 pages without reworking them.

**Delivers:**

- Run Detail actions (ACTN-02..03): "Add agent session" with role picker, enabled only while the run executes, surfacing the approved-plan gate inline and the budget it will consume; kill on agent-session rows (Run Detail + agent rail) with a reason field landing in the append-only error note, the API's 409 race semantics ("another actor finalized the session") surfaced verbatim
- Plans-sync (ACTN-04): one click on the Plans page header showing the sync result (plans added/updated) — `syncAppPlans` finally consumed by a page; the app-card instance of this action lands with the registry cards in Phase 9
- Utility actions (ACTN-05): canary run-now, MCP probe (read-only tools/list + one bounded call showing latency + raw response, no budget), force orphan sweep
- Shared action contract (ACTN-06): consequence sentence before commit, confirm only where destructive, server-ack waits (no optimistic UI), API errors verbatim; existing v1.0 actions (approve/reject, quarantine, corpus promote, alert acknowledge) verified non-regressed

**Requirements covered:** ACTN-02..06 (5/27)

**Success criteria:**

- "Add agent session" on Run Detail offers the role picker only while the run executes, states the approved-plan gate and the budget it will consume, and the new session appears in the agent rail consuming exactly that budget
- Killing a session from Run Detail or the agent rail writes the operator's reason into the append-only error note; when another actor finalized the session first, the API's 409 sentence surfaces verbatim
- Plans-sync from the Plans page header completes without leaving the page and reports what was added/updated
- Canary run-now, MCP probe (latency + raw response shown, zero budget consumed), and force orphan sweep each complete with visible results
- Every mutating action follows the shared contract — consequence sentence before commit, server-ack wait, verbatim API errors — and approve/reject, quarantine, corpus promote, and alert acknowledge still work unchanged

**Depends on:** Phase 6 (budget gate, audit pattern, confirm pattern, permission wiring)
**Guardrails:** v1.0 pages are additive-only — actions attach, nothing reworked. MCP probe is read-only and budget-free. Server-ack waits everywhere; no optimistic UI.
**Plans**: TBD
**UI hint**: yes

### Phase 9: Apps registry completion and Jobs Observatory

**Goal:** The registry becomes a managed surface and River jobs become observable and governable — with the `admin.e2ai.admin` permission rung's first real consumers verified end-to-end in staging.

**Delivers:**

- Registry cards (APPS-04): per-app health summary (runs today, last verdict, agent mini-rail), config state (edge-sweep, thresholds, quarantine/triage counts), detail view; cards carry the plans-sync action established in Phase 8
- Add/edit wizard (APPS-05..06): registration fields + flake thresholds + edge-sweep toggle; secrets as 1Password references only — never raw values — with server-side resolution verified before save; duplicate repo/workflow/environment surfaces the unique-constraint error inline; delete is a two-step typed-slug confirm with dependency warning, keeping run history and only stopping registration + ingest matching; honors the register-app deep-link pre-fill contract from Phase 7 (INBX-03)
- Admin rung's first consumers (APPS-07, JOBS-03): registry CRUD + queue pause/resume gated by `admin.e2ai.admin`, with the BFF→header→permission chain verified in staging
- Jobs observatory (JOBS-01..02): River jobs list with state/queue/kind filters and cursor pagination plus a canary card (inert vs active, pinned image, last canary verdict); guarded cancel (River cancel paired with a guarded run-state transition + error note, idempotent re-cancel); retry offered only for default-queue kinds (report, plans sync, derive draft) — run-level retry routes to the manual re-run flow (TRIG-04)
- Queue pause/resume (JOBS-03): persistent pause banner naming the blast radius (pausing `default` silently stalls wait-rollout), state survives restarts, pause state reflected in the Command Center KPI header

**Requirements covered:** APPS-04..07, JOBS-01..03 (7/27)

**Success criteria:**

- A new app registers through the wizard with a vault-ref secret the server verified resolves; a duplicate repo/workflow/environment combination shows the constraint error inline; no raw secret value is ever accepted or displayed
- Deleting an app requires typing the slug, warns about dependencies, keeps run history, and only stops registration + ingest matching
- The jobs page filters by state/queue/kind with cursor pagination; cancelling a job leaves a coherent run state + error note; retry is offered only for default-queue kinds, and a run-level retry routes to the manual re-run flow instead
- Pausing a queue shows a persistent banner warning about wait-rollout stalls (for the `default` queue), survives an api restart, and flips the Command Center KPI pause state
- A non-admin is refused at the gate for registry CRUD and queue pause/resume while an admin's actions succeed — the BFF→header→permission chain verified in staging

**Depends on:** Phase 6 (permission wiring, Command Center KPIs, synthetic re-run flow) + Phase 8 (action contract; plans-sync anchor on cards)
**Guardrails:** Two flagged planning decisions bound this phase (research gaps): registry source of truth (YAML-canonical vs DB-canonical `registered_apps`) and River cancel propagation (verify the runner client's listener control-event config, or defer `JobCancel` entirely — retry + observe first). No raw secret entry/display anywhere. New actions stay additive to v1.0 pages.
**Plans**: TBD
**UI hint**: yes

## Roadmap-Level Notes

- **Order is deliberate:** deterministic spine first (P1), human-supervised autonomy second (P2), agentic execution third (P3) — agents must beat the deterministic baseline and inherit verdict/report plumbing, not bootstrap their own. Mobile (P4) only after the web loop proves out; governance (P5) needs accumulated history to be meaningful.
- **Every phase ships reports:** the report-only invariant holds from Phase 1 commit one.
- **Approval gating scales with autonomy:** Phase 2 gates plans before agents exist; Phase 3 consumes that gate as its first safety rail.
- **Budgets land with agents, not after:** Phase 3 is unsafe without layered caps and the kill switch.

## Verification Approach

- Phase-level: gsd-verifier goal-backward check against the success criteria above; UI phases via live-DOM verification
- Cross-phase (v1.0): integration check after Phase 3 (deterministic + agentic runs coexist on the same report surface) and after Phase 5 (second app + MCP consumer)
- Cross-phase (v1.1): staging verification of the BFF → header → permission chain when the first consumer of each permission rung lands (edit rung in Phase 6, admin rung in Phase 9); integration check after Phase 8 that every cockpit action coexists with v1.0 pages without regression

---
*Roadmap generated: 2026-09-19 — v1.1 Admin Cockpit milestone added 2026-09-22*
*Aligned with: v1.1 SUMMARY.md 4-phase skeleton, REQUIREMENTS.md coverage (v1.0: 45/45 shipped; v1.1: 27/27 mapped, 0 unmapped)*
