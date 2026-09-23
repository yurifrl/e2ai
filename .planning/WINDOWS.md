---
schema_version: 1
open_count: 2
waived_count: 0
fixed_count: 0
total_count: 2
last_updated: 2026-09-23T09:14:30.093Z
---

# Broken Windows Ledger

> Cross-phase defect register. With `workflow.windows_enforce` enabled, `/gsd-ship` blocks while `open_count > 0`.
> Waive with `gsd-tools windows waive <id> "<reason>"` (reason required).
> Mark fixed with `gsd-tools windows fixed <id>`.

| id | phase | kind | file | line | description | status | reason | recorded_at | resolved_at |
|----|-------|------|------|------|-------------|--------|--------|-------------|-------------|
| 1 | 6 | stub | web/src/features/cockpit/KpiCards.tsx |  | KPI zone grid is an intentional shell: cards mount with Plan 04 endpoints (plan 06-01 documents the split) | open |  | 2026-09-23T09:14:29.960Z |  |
| 2 | 6 | deviation | web/src/lib/api.ts |  | pauseCockpit mirrors locked http.go body (reason-only, route picks direction) instead of plan's {paused, reason} sketch | open |  | 2026-09-23T09:14:30.093Z |  |

````json
[
  {
    "id": 1,
    "kind": "stub",
    "phase": "6",
    "file": "web/src/features/cockpit/KpiCards.tsx",
    "line": null,
    "description": "KPI zone grid is an intentional shell: cards mount with Plan 04 endpoints (plan 06-01 documents the split)",
    "status": "open",
    "reason": "",
    "recorded_at": "2026-09-23T09:14:29.960Z",
    "resolved_at": null
  },
  {
    "id": 2,
    "kind": "deviation",
    "phase": "6",
    "file": "web/src/lib/api.ts",
    "line": null,
    "description": "pauseCockpit mirrors locked http.go body (reason-only, route picks direction) instead of plan's {paused, reason} sketch",
    "status": "open",
    "reason": "",
    "recorded_at": "2026-09-23T09:14:30.093Z",
    "resolved_at": null
  }
]
````
