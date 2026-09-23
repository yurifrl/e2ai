---
phase: 06-cockpit-trigger-command-center
reviewed: 2026-09-23T22:31:44Z
depth: standard
files_reviewed: 49
files_reviewed_list:
  - api/cmd/e2ai-api/main.go
  - api/internal/app/migrations/0010_cockpit.sql
  - api/internal/app/migrations/migrations_test.go
  - api/internal/app/postgres.go
  - api/internal/cockpit/agents.go
  - api/internal/cockpit/audit.go
  - api/internal/cockpit/budget.go
  - api/internal/cockpit/budget_test.go
  - api/internal/cockpit/feed.go
  - api/internal/cockpit/feed_test.go
  - api/internal/cockpit/http.go
  - api/internal/cockpit/kpis.go
  - api/internal/cockpit/service.go
  - api/internal/cockpit/simulate.go
  - api/internal/cockpit/simulate_http.go
  - api/internal/cockpit/simulate_test.go
  - api/internal/cockpit/state.go
  - api/internal/cockpit/state_test.go
  - api/internal/cockpit/trigger.go
  - api/internal/cockpit/trigger_gates.go
  - api/internal/cockpit/trigger_http.go
  - api/internal/cockpit/trigger_test.go
  - api/internal/ingest/dedupe.go
  - api/internal/ingest/http.go
  - api/internal/plans/http.go
  - api/internal/platform/github/client.go
  - api/internal/report/comment.go
  - api/internal/report/comment_test.go
  - api/internal/report/page.go
  - api/internal/runs/queries.go
  - api/internal/suite/execute.go
  - api/internal/suite/resolution_test.go
  - web/admin.module.json
  - web/src/app/routes.tsx
  - web/src/features/cockpit/AgentRail.tsx
  - web/src/features/cockpit/CommandCenterPage.tsx
  - web/src/features/cockpit/KpiCards.tsx
  - web/src/features/cockpit/SimulateCard.tsx
  - web/src/features/cockpit/TriggerPanel.tsx
  - web/src/features/plans/PlanDetailPage.tsx
  - web/src/features/runs/RunDetailPage.tsx
  - web/src/i18n/locales/en-US.json
  - web/src/i18n/locales/es.json
  - web/src/i18n/locales/pt-BR.json
  - web/src/i18n/locales/ro.json
  - web/src/lib/api.ts
  - web/src/lib/types.ts
  - web/src/main.tsx
  - web/vite.config.ts
findings:
  critical: 2
  warning: 6
  info: 4
  total: 12
status: issues_found
---

# Phase 6: Code Review Report

**Reviewed:** 2026-09-23T22:31:44Z
**Depth:** standard
**Files Reviewed:** 49
**Status:** issues_found

## Summary

Phase 6 (cockpit control plane, manual trigger, webhook simulate, Command Center read model, web surface) was reviewed at standard depth across all 49 changed files: the cockpit domain (state, audit, budget, feed, KPIs, agent rail, trigger, simulate), the HTTP boundaries, the ingest/report/suite seams the cockpit drives, migration 0010, and the full web module.

The substrate is genuinely strong: pause state and audit commit in one transaction (AUDT-01 holds on the trigger and pause paths), the insert-as-gate partial unique index with its fallback lookup is correctly formed and matches migration 0010's predicate exactly, the budget policy is shared verbatim between gate and KPI card, the feed uses keyset pagination with parameterized per-branch qualification, the simulate HMAC path self-verifies with a constant-time compare, artifact path sanitization is a strict charset whitelist, the report token uses `hmac.Equal`, locale parity is exact (95 keys in all four languages), and the runner entrypoint quotes every expansion.

However, two critical defects break the phase's headline features: the Command Center trigger panel's **default** head-source ("latest") submits `head_sha: ''` to a server that requires it for plain mode — the default flow 422s 100% of the time — and the Command Center feed embeds the raw lowercase DB run status in summaries that the web client matches against an uppercase vocabulary, so **every** run event renders with the wrong chip and never shows the live/final tone. Six warnings cover a validated-but-unformatted `head_sha` reaching `git fetch` argv, a simulate audit that is not transactional with the run insert, a free-form environment that bypasses the app's registered environment, a pause/budget TOCTOU window, feed page-boundary duplicates, and unbounded request bodies.

No critical issues were found in the runner entrypoint (quoted `git checkout "$sha"` defeats shell injection), the docker argv assembly (no shell), the pause gate's transactional attribution, the audit record shape, or the artifact/comment sanitizers.

## Critical Issues

### CR-01: Default trigger path always fails — panel's "latest" head source has no server support

**File:** `web/src/features/cockpit/TriggerPanel.tsx:101-103,246,252` and `api/internal/cockpit/trigger_gates.go:36-39,214-215`
**Issue:** The panel defaults `headSource` to `'latest'` for plain mode (only `reproduce` forces `'pin'`). With `headSource === 'latest'`, `headSha` evaluates to `''` (line 246) — yet `canArm` is true because line 252 admits `headSource === 'latest'` — so the panel arms and submits `head_sha: ''`. The server's `validate()` requires a non-empty `head_sha` for plain mode (`trigger_gates.go:36-39`: "head_sha is required for a plain manual run"), and `resolveHead`'s plain-mode branch just returns `req.HeadSHA` (lines 214-215) — there is no latest-resolution path despite the wired `HeadResolver`. Net effect: the Command Center's default, first-touch trigger flow fails with 422 every single time; the operator must discover that the advertised "latest" option is unusable and switch to "pin" manually.
**Fix:** Make one side honor the other. Preferred (matches the UI's intent and uses the already-wired resolver) — server-side: in `resolveHead`, treat empty `head_sha` in plain mode as "resolve latest" via `s.Heads` (fail-closed like retest), and relax `validate()` to require head_sha only when the panel pins. Client-side alternative: default `headSource` to `'pin'` and render "latest" only if the server gains a resolution path.

```go
// trigger_gates.go — resolveHead, plain branch
default:
    if req.HeadSHA == "" {
        return s.Heads.Resolve(ctx, a.Owner, a.Repo) // fail-closed, like retest
    }
    return req.HeadSHA, nil
```

### CR-02: Feed run events never render status chips or live tone (case mismatch between server summary and client vocabulary)

**File:** `api/internal/cockpit/feed.go:134-139` and `web/src/features/cockpit/CommandCenterPage.tsx:284-286,300-303`
**Issue:** The feed's runs branch embeds the raw DB status in the summary: `r.environment || ' run ' || r.workflow_run_id::text || ' — ' || r.status`. `runs.status` stores the lowercase enum (`'queued'`, `'executing'`, …). The web client derives `statusWord = event.summary.split(' — ').pop()` (line 284) and tests `statusWord in RUN_TONE` (line 285) whose keys are the **uppercase** wire vocabulary (`'QUEUED'`, `'EXECUTING'`, …). The lookup therefore fails for every run event: `statusIsRun` is always false, so run rows fall through to the generic `KindChip` (session-style chip, lines 300-303), `StatusChip` never renders, and the live/final tone (line 286) never applies. The phase's headline activity stream mis-renders every run event it displays. Note the runs REST wire uppercases server-side (`report/page.go` `wireStatus` → `strings.ToUpper`), which is exactly the vocabulary the client matches — the feed is the one source that skipped that normalization.
**Fix:** Normalize server-side so the summary carries the wire vocabulary:

```sql
case when r.environment <> '' then
    r.environment || ' run ' || r.workflow_run_id::text || ' — ' || upper(r.status)
else 'run ' || r.workflow_run_id::text || ' — ' || upper(r.status) end as summary
```

(or match case-insensitively in `FeedRow`). Add a feed_test assertion that the summary's status suffix round-trips through the web's `RUN_TONE` vocabulary so the coupling can't silently drift again — today the two sides are joined only by an em-dash-split convention.

## Warnings

### WR-01: `head_sha` is accepted unvalidated and flows into `git fetch`/`git checkout` argv

**File:** `api/internal/cockpit/trigger_gates.go:36-39`; `api/internal/suite/execute.go` (env assembly); `runner/entrypoint.sh:34-35`
**Issue:** The server enforces only non-emptiness for plain-mode `head_sha` — no charset/length check. The web client validates `/^[0-9a-f]{7,40}$/i` (TriggerPanel:252), but the API trusts any string. The value travels as `E2AI_SHA` into the container and becomes `git fetch --filter=blob:none origin "$sha"` and `git checkout --detach "$sha"`. Quoting defeats shell injection entirely (good), but git parses option-shaped arguments at the refspec/revision position (`--upload-pack=…`, `--config=…`, …). With the https origin this is inert, but with a local/file remote those options execute binaries inside the runner container — which holds `E2AI_GITHUB_TOKEN`. Defense-in-depth gap on an operator-authenticated surface.
**Fix:** Validate server-side where the value is born — add `^[0-9a-f]{7,40}$` (case-insensitive) to `validate()` for plain mode; optionally add `--` before the rev in `git checkout --detach` where git supports it.

### WR-02: Simulate's audit write is not transactional with the run insert — AUDT-01 contract violated on this path

**File:** `api/internal/cockpit/simulate.go:211,328-350`
**Issue:** `Simulate` commits the run through `Store.Ingest` (line 211) and only then opens a separate transaction for `AppendAudit` (lines 328-350). If the audit tx fails, the error returns as a 500 while the run row persists — the mutation landed without its audit record, contradicting the package contract stated in `audit.go`/`service.go` ("every cockpit mutation appends [the audit] in the same transaction as its domain write"). Worse, the natural operator retry starts a new simulate with a fresh `cockpit_run_ids` workflow id, which ingest accepts as a new run — duplicating runs on the path that is specifically supposed to be a faithful rehearsal of real webhooks. The trigger and pause paths do this correctly (single tx, `trigger.go:165-226`, `state.go:58-109`); simulate structurally cannot via the `Store.Ingest` seam as built.
**Fix:** Extend the ingest seam to carry attribution (actor/reason/outcome) in `RunSeed` so ingest's own transaction appends the audit row atomically with the run insert — the same seam the sweep uses. If that is out of reach this phase, at minimum document the deviation at both sites and make the audit failure surfaced (it currently reads to operators as "simulate failed" while its side effect landed).

### WR-03: Manual-run `environment` is free-form and unchecked against the app's registered environment

**File:** `api/internal/cockpit/trigger_gates.go:26-39,233-236`
**Issue:** `validate()` requires only non-empty environment; `insertManualRun` stamps `req.Environment` verbatim onto the run row. But `registered_apps` is unique per `(owner, repo, workflow, environment)` — `app_id` already pins the environment, and the trigger even loads `a.Environment` from the row it just fetched. An operator typo ("prodction") creates a run in a phantom environment that no app row backs: it counts toward per-app KPIs and budget (both keyed by app id, so the numbers stay consistent) while carrying a false environment label into the feed, run detail, and any downstream consumer that renders or groups by environment.
**Fix:** Reject a mismatch before the transaction:

```go
if req.Environment != a.Environment {
    return fmt.Errorf("%w: environment %q is not the app's registered environment %q",
        errInvalidRequest, req.Environment, a.Environment)
}
```

### WR-04: Pause and budget gates are read outside the trigger transaction (TOCTOU)

**File:** `api/internal/cockpit/trigger.go:165` (Begin); gates precede it in `trigger.go`/`trigger_gates.go`
**Issue:** `checkPaused` and budget `Evaluate` run before `tx.Begin`. A pause (or budget-consuming run) that commits between the gate reads and the insert is bypassed by this trigger: the insert-as-gate partial index arbitrates only one-manual-in-flight, not pause state. The window is small (a few queries), but the semantic claim is "the paused arm blocks manual triggers," and the racing request is precisely the one an operator paused to stop. The gate-reads-outside-tx design is documented for the duplicate case; the pause race is not acknowledged anywhere.
**Fix:** Re-check the single-row `cockpit_state.paused` inside the transaction just before the insert (one cheap indexed read), and refuse with `ErrPaused` if it flipped. Budget re-check can stay coarse, but say so in the comment.

### WR-05: Feed load-more duplicates rows after a live refresh

**File:** `web/src/features/cockpit/CommandCenterPage.tsx:76-79,253-254`
**Issue:** The page list is `[firstPage, ...older]` flattened with `flatMap` and no id-level dedupe (line 79). The first page re-polls every 10s against a shifting keyset while older pages hold fixed cursors; when new events push rows across the first/older boundary, the same event id appears in the first page's tail and the older page's head — duplicate rows render (and `key={e.id}` at line 254 then collides). The server-side keyset design is correct; the client just doesn't reconcile.
**Fix:** Dedupe when composing entries: `[...new Map(entries.map((e) => [e.id, e])).values()]` in the `entries` useMemo (or filter each older page against the ids already present).

### WR-06: Cockpit POST endpoints read request bodies without a size cap

**File:** `api/internal/cockpit/trigger_http.go` (decode), `api/internal/cockpit/simulate_http.go` (decode), `api/internal/cockpit/http.go:90` (pause mutate decode)
**Issue:** All three boundaries decode `r.Body` directly with `json.NewDecoder` and no `http.MaxBytesReader`, so a caller with edit/view permissions can post an arbitrarily large body that Go buffers in memory before the (cheap) field checks. The pause path additionally discards the decode error (`_ =`), so malformed JSON silently becomes an empty reason. Authenticated surfaces only, but the pattern invites memory pressure and hides malformed payloads.
**Fix:** Wrap the body once per handler before decode — `r.Body = http.MaxBytesReader(w, r.Body, 64<<10)` — and treat decode errors as 422 request-shape violations instead of `_ =`.

## Info

### IN-01: Simulate wire uses PascalCase keys because `SimulateRequest` has no json tags

**File:** `api/internal/cockpit/simulate.go:50-53`; `web/src/lib/types.ts` (CockpitSimulateRequest comment); `web/src/lib/api.ts`
**Issue:** `SimulateRequest` fields carry no `json:"..."` tags, so encoding/json matches Go field names (`AppID`, `PayloadShape`) — documented deliberately in the web types as a locked quirk. It remains an asymmetry with the trigger's snake_case wire and a trap for the next endpoint built in the same package.
**Fix (later phase):** Add json tags to `SimulateRequest` and update `types.ts`/`api.ts` in the same commit, or leave as-is with the existing comments kept accurate.

### IN-02: Budget endpoint maps an unknown app to 500 instead of 404

**File:** `api/internal/cockpit/kpis.go` (BudgetHTTP / Budget lookup)
**Issue:** A bad `app_id` that parses but matches no registered app surfaces as `errInvalidRequest`-shaped lookup failure → 500 "budget read failed" and a logged error, while the sibling surfaces return typed not-found errors. Not a correctness break, but it pollutes error telemetry for a user-input mistake.
**Fix:** Distinguish "unknown app" in the Budget lookup and return 404 from `BudgetHTTP`.

### IN-03: `audit_events` append-only is by convention, not enforced by schema

**File:** `api/internal/app/migrations/0010_cockpit.sql` (audit_events)
**Issue:** Comments describe the audit trail as append-only, but no schema mechanism (revoked UPDATE/DELETE, trigger guard) enforces it for other migrations/roles. The in-code paths only ever INSERT — the guarantee is currently social, not structural.
**Fix (optional):** Add a schema-level guard (`revoke update/delete` from the app role, or a before-update/delete trigger raising an exception) if the append-only claim is meant to be load-bearing.

### IN-04: Two simultaneous arm-state polls on the same page

**File:** `web/src/features/cockpit/TriggerPanel.tsx` (state poll) and `web/src/features/cockpit/KpiCards.tsx` (state poll)
**Issue:** The Command Center page polls `GET /api/cockpit/state` twice per 10s cycle once the trigger panel is open — KpiCards polls it for the banner and the panel polls it independently. Harmless, but it doubles the load on the state endpoint and invites drift between the two rendered copies of `paused`.
**Fix (optional):** Lift the `getCockpitState` poll into the page and pass `state` down as a prop (both components already receive props from CommandCenterPage).

---

_Reviewed: 2026-09-23T22:31:44Z_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: standard_