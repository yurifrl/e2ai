---
phase: 06-cockpit-trigger-command-center
plan: 05
type: execute
status: completed
---

# Plan 05 — Cockpit Trigger + Simulate (Web) — Summary

## What landed

### New components

- **`web/src/features/cockpit/TriggerPanel.tsx`** (587 lines) — the inline expanding
  trigger panel (TRIG-01..05), one component mounted three ways:
  - **Command Center pagehead** (`initialMode=''`): plain-mode picker — app → approved
    plan version → head source (latest / pin-commit with 7–40 hex validation) →
    environment → optional reason.
  - **PlanDetailPage** (`presetPlan` + `presetVersions`): "Trigger run" button with the
    version preselected, app locked to the plan's registered `app_id` (button hidden when
    the plan row has no registered app).
  - **RunDetailPage re-run pair** (`initialMode='reproduce'|'retest'` + `sourceRun`):
    reproduce pins the source run's SHA (head_sha sent empty, server uses
    `source_run_id`); retest resolves head server-side (`head_sha` sent empty per
    `trigger_gates.go` — retest rejects a non-empty head_sha).
  - Two-step arm: pick form → armed confirm card (plan identity + hash-pinned, run shape
    from plan surfaces + client-side numbered-step count, layered budget line), then a
    single "Confirm trigger" POST.
  - Pre-flight: `getCockpitState` (paused → disabled + reason shown verbatim) and
    `getCockpitBudget(appId)` strip — zero-ceiling ("cap is $0"), exhausted, warn, and
    no-cap states per BUDG-03; budget refusal renders the server's 409 message verbatim.
  - Plans fetched via `listPlans({status:'approved'})` filtered client-side by
    `app_id`; versions filtered to `status==='approved'`, draft rows shown disabled with
    "draft (not triggerable)".
  - Success block: outcome line + "open run #{workflow_run_id}" link to Run Detail.

- **`web/src/features/cockpit/SimulateCard.tsx`** — webhook simulate card (ACTN-01 web
  half) in the right rail: app select (feed-derived app set), payload-shape select
  (`workflow_run` — the one real ingest flavor), optional reason, single-click POST to
  `/api/cockpit/simulate`; result block renders the ingest outcome vocabulary
  (accepted / recorded / already-ingested) with "open run #{id}" link on accepted.

### Page composition

- **`CommandCenterPage.tsx`**: "Trigger run" CTA in the pagehead behind
  `hasPermission('admin.e2ai.edit')`; panel mounts below the pagehead (no modal; the
  feed keeps polling); `SimulateCard` in the right rail (edit-gated).
- **`COCK-01` route cutover**: `routes.tsx` index route now renders
  `CommandCenterPage`; runs remain at `/e2ai/runs`. Run Detail back-link points at
  `/e2ai/runs` (was the old index).
- **`RunDetailPage.tsx`**: re-run pair (reproduce / retest) + `triggered_by`
  attribution line ("Triggered by {operator} · {at} · manual") when present (AUDT-01).
- **`PlanDetailPage.tsx`**: "Trigger run" button + panel mount (read-only otherwise).

### Wire + data

- **`web/src/lib/types.ts`**: `CockpitTriggerRequest` / `CockpitTriggerAck` /
  `CockpitSimulateRequest` / `CockpitSimulateAck` added, locked verbatim to
  `api/internal/cockpit/{trigger,trigger_http,simulate,simulate_http}.go` (an accidental
  duplicate declaration block was removed; single source of truth).
- **`web/src/lib/api.ts`**: `postCockpitTrigger`, `postCockpitSimulate` (Endpoints enum).

### Config / dev identity (dev_auth_note)

- **`web/src/main.tsx`**: standalone-dev mock roles — admin/editor now carry
  `admin.e2ai.edit` (viewer stays view-only).
- **`web/vite.config.ts`**: `/e2ai/api` proxy forwards
  `X-Permissions: admin.e2ai.view admin.e2ai.edit` so cockpit mutations don't 403 in
  standalone dev.
- **`web/admin.module.json`**: `permissions.edit: ["admin.e2ai.edit"]` declared;
  new `cockpit` nav command (module entry, route `/e2ai`, icon `layout-dashboard`,
  localized 4 locales) added ahead of `runs`.

### i18n

- 55 new keys per locale (`cockpit.trigger.*` ×49, `cockpit.simulate.*` ×13,
  `detail.rerun*` ×3, `detail.triggeredBy`) in all four locales
  (en-US / pt-BR / es / ro). Cross-locale parity verified programmatically; every
  `t('...')` reference in `src/features/**` resolves. A clobbered
  `cockpit.feed.sessionChip` key was restored in all four files.

## Verification

- `npx tsc --noEmit` → clean (one unused-setter warning fixed by making `mode` a
  mount-time const).
- Locale parity + key-resolution checks → OK (scripted).
- `admin.module.json` parses; manifest now declares the edit rung.
- Full-stack browser walkthrough (trigger a run as an editor) is the end-of-phase Tilt
  verification owned by the phase orchestrator; the Okta grant of `admin.e2ai.edit` is
  verified in the staging console per Plan 05 verification notes.

## Notes / decisions

- Mode is fixed per mount (picker = plain; run pair = reproduce/retest); only the head
  source (latest/pin) is user-changeable — matches the UI-SPEC zone-B flow and keeps the
  armed confirm honest.
- Plan run-shape preview counts numbered steps client-side with the same
  `^\s*\d+\.\s` shape `plans/parse.go` uses; step authority stays server-side.
- Budget formatting mirrors KpiCards' millicents→dollars convention.
