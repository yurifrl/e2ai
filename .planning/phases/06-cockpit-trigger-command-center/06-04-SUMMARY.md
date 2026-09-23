---
phase: 06-cockpit-trigger-command-center
plan: 04
subsystem: web-cockpit
tags: [cockpit, command-center, react, i18n, read-models]
requires:
  - "Plan 03 read-model endpoints (GET /api/cockpit/{feed,kpis,agents,budget})"
  - "Plan 01 cockpit substrate (CockpitState DTO, KpiCards pause shell)"
affects:
  - "Plan 05 (trigger panel + landing swap mount CommandCenterPage at /e2ai)"
tech-stack:
  added: []
  patterns:
    - "props-down KPI polling (page polls once, cards render)"
    - "keyset-cursor Load more mirroring server FeedPage.cursor"
    - "app filter chips derived from polled feed entries (no extra endpoint)"
key-files:
  created:
    - web/src/features/cockpit/AgentRail.tsx
    - web/src/features/cockpit/CommandCenterPage.tsx
  modified:
    - web/src/lib/types.ts
    - web/src/lib/api.ts
    - web/src/features/cockpit/KpiCards.tsx
    - web/src/i18n/locales/en-US.json
    - web/src/i18n/locales/es.json
    - web/src/i18n/locales/pt-BR.json
    - web/src/i18n/locales/ro.json
decisions:
  - "trigger_source union already carried SWEEP/MANUAL/SIMULATE (Plans 02/03 extended it) — union verified, not re-extended"
  - "app filter chips enumerate distinct app_ids present in the polled feed (no apps-list endpoint exists; zero new server contracts per plan contract)"
  - "app identity renders as short-uuid prefix (first 8 chars) — no name mapping on the wire"
  - "admin-ui registry components not imported; house-built equivalents (chips, banners) consistent with the repo's existing pattern of plain Tailwind + local chips"
metrics:
  duration: ~45 min
  completed: 2026-09-23
status: complete
---

# Phase 6 Plan 4: Command Center web read surface Summary

Command Center web read surface: locked wire DTOs + read clients, four KPI data cards around the Plan 01 pause shell, agent rail with stuck flags and Run Detail links, and the sketch 001-A Command Center page composing feed + filters + keyset paging — all copy in four locales, build + manifest green.

## What Was Built

### Task 1 — Wire DTOs + read clients (3cfa26f6)

`web/src/lib/types.ts` gained the Plan 03 read-model DTOs, each locked verbatim to the Go JSON tags:

- `CockpitFeedEvent` (`id, kind, created_at, app_id, run_id?, summary`) + `CockpitFeedPage` (`entries, cursor?`) and `CockpitFeedKind = 'run' | 'agent_session' | 'alert'` (feed.go)
- `CockpitKpis` (`runs_today, pass_rate_today, budget{spent_today, caps_sum, uncapped_count}, pending_approvals`) (kpis.go)
- `CockpitAgentSession` (`run_id, role, status, heartbeat_age_seconds, stuck`) (agents.go)
- `CockpitBudget` (`app_id, cap_kind, cap_millicents, capped, spent_today, remaining_millicents, remaining_capped, refused, reason?`) (kpis.go PerAppBudget — the gate's own verdict, not a recomputation)

`web/src/lib/api.ts` gained `getCockpitFeed({limit, appId, cursor})`, `getCockpitKpis(appId?)`, `getCockpitAgents(appId?)`, `getCockpitBudget(appId)` over the existing `getJson`/`ApiError` pipeline. The `trigger_source` union was verified — Plans 02/03 already extended it to `'WEBHOOK' | 'RECONCILE' | 'SWEEP' | 'MANUAL' | 'SIMULATE'` — so it was extended, not rewritten, per plan success criteria.

### Task 2 — KPI data cards + agent rail (fcdcf835)

`web/src/features/cockpit/KpiCards.tsx` extended from the Plan 01 pause-control shell with the four data cards in the 5-column header grid (arm-state card first):

- **Runs today** — numeral only; the locked wire carries no yesterday aggregate, so the sketch's "vs yesterday" delta line is omitted (delta shown as "—", per UI-SPEC no-baseline rule).
- **Pass rate today** — `Math.round(pass_rate_today * 100)%` with muted "of completed runs" sub-line.
- **Pending approvals** — links to `/e2ai/approvals` (Approvals queue stays the only approval path), amber warning tone when > 0.
- **Budget spend vs cap** — mono `formatMillicents` dollars (`$${(m / 100_000).toFixed(4)}`, house pattern from TrendsPage/RunReportPage); capped → `spend / cap`; uncapped → explicit `t('cockpit.kpi.budgetNoCap', {count})` — never a fabricated 0 cap; amber border at ≥ 80% utilization; uncapped-apps note line.

Cards are props-driven (`kpis`, `kpisError`): the Command Center page polls `getCockpitKpis` once and passes the payload down — one poll, one payload per CONTEXT decision 4.

`web/src/features/cockpit/AgentRail.tsx` (new): the live session rail — one row per non-terminal session, oldest heartbeats first (server order), status dot (busy blue / stuck amber, >90s server predicate surfaced verbatim), role name (Navigator/Vision Judge/Flakiness Analyst via `cockpit.rail.role.*`), status text, mono heartbeat age, STUCK flag chip, and every row linking to its Run Detail page (`/e2ai/runs/{run_id}`) per plan requirement. Empty and error states resolve through locales.

### Task 3 — Command Center page (7a54a9fb)

`web/src/features/cockpit/CommandCenterPage.tsx` (new): sketch 001-A variant A anatomy — pagehead (h1 "Command Center" + locked subline "The QA arm, always reporting. Never blocking."), KpiCards zone, then the locked two-column grid (feed `1fr` + rail `320px`):

- **Polling**: `getCockpitFeed` / `getCockpitKpis` / `getCockpitAgents` each on the RunsListPage 10s analog via `usePolling` (document-hidden pause built into the hook); `CockpitState` stays inside KpiCards' own poll — no second state fetch.
- **Keyset pagination**: first page from the poll, "Load more" appends `getCockpitFeed(…, cursor)` pages; cursor chain follows each page's opaque cursor; no OFFSET.
- **App filter chips**: "All apps" + one chip per distinct `app_id` present in the polled feed entries; switching a chip re-keys the feed/rail polls with `app_id` and resets appended pages.
- **Feed rows**: mono time · status chip (run summaries end in the wire status word, mapped through the house `StatusChip` taxonomy via a full `Record<RunStatus, …>` tone table) · kind chip for session/alert rows · LIVE pulse chip for in-flight statuses (QUEUED/WAITING_ROLLOUT/PROVISIONING/EXECUTING/REPORTING) · `{app} · {summary}` · chevron. Rows are `aria-expanded` buttons whose inline detail strip shows the full summary, Run Detail link (run/session rows) or View Trends link (alert rows → `/e2ai/trends`).
- **Accessibility**: `aria-live="polite"` region announcing entry count; labelled detail strips; buttons not bare divs.

Not mounted in routes — the landing swap to `/e2ai` is Plan 05's contract; nothing re-routes in this plan (manifest untouched, validated green).

### Locales (T2 + T3)

All visible strings resolve through `t('e2ai.cockpit.*')`; 16 new keys in Task 2 (kpi + rail) and 14 in Task 3 (page + feed) added to **en-US, es, pt-BR, ro** — verified: every `t()` key used in the three cockpit files exists in all four locale files (41 keys cross-checked, 0 missing per locale, JSON syntax valid).

## Verification

- `tsc` (web/): **green** after each task.
- `bun run build` (vite): **green** (pre-existing >500kB chunk warning only).
- `bun run validate:manifest`: **green** — `manifest OK: @admin/e2ai (routeBase /e2ai)`, vendored-schema fallback per prior waves.
- i18n cross-check: all used `cockpit.*` keys present in all four locales; `python3 json.load` valid on all four files.
- Server endpoints were verified as existing + mounted in Plan 03 (web consumes them; no server change in this plan).

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] deps not installed in web/**
- **Found during:** Task 1 verification
- **Issue:** `web/node_modules` absent; `npx tsc` resolved a non-project stub.
- **Fix:** `bun install` in web/ (lockfile unchanged, 277 packages); used `./node_modules/.bin/tsc`.
- **Files modified:** none (node_modules is gitignored).

**2. [Rule 1 - Bug] ts-set-map rule violation in first CommandCenterPage draft**
- **Found during:** Task 3
- **Issue:** string-keyed set-shaped constants written as plain object literals.
- **Fix:** rewrote as `Record<RunStatus, RunTone>` full-tone table + `Record` membership via `in`.
- **Files modified:** web/src/features/cockpit/CommandCenterPage.tsx (committed in 7a54a9fb).

### UI-SPEC deltas (documented, no scope change)

- **admin-ui registry components** (Skeleton/Alert/EmptyState): the repo imports nothing from `@nsxbet/admin-ui` anywhere in `web/src`; introducing the first import is untestable against house patterns here. House-built equivalents (existing chips, Tailwind banners) follow the UI-SPEC's own "house chips are canonical" rule. Registry-card trigger mount is Plan 05's surface anyway.
- **App filter chips enumerate feed app_ids** rather than one-chip-per-registered-app: no apps-list endpoint exists and the plan contract forbids new server contracts; chips derive from polled feed entries (the "All apps" + per-app chip semantics preserved).
- **Runs-today delta line omitted**: the locked wire carries no yesterday aggregate; rendering a fabricated 0 would violate the no-fabrication invariant (UI-SPEC's "delta — when no prior-day baseline" rule covers this).
- **App identity as short-uuid**: no name mapping exists on any wire the module exposes; rows/chips show the first 8 uuid chars.

## Known Stubs

None. All components render from polled API truth; the page is intentionally unmounted from routes (Plan 05 performs the landing swap — not a stub).

## Notes for Plan 05

- `CommandCenterPage` exports `CommandCenterPage` — mount it at the `/e2ai` route index and swap the manifest landing.
- Trigger panel mounts below the pagehead (`Trigger run` CTA); the panel contract and 409 budget/pause copy land there with `getCockpitBudget` already exported from `api.ts`.
- KpiCards is props-driven; if Plan 05 mounts it standalone, pass `kpis={null}` or poll as the page does.
