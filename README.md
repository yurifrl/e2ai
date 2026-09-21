# e2ai

Autonomous e2e testing control plane: declarative plans + a governed agent
roster (navigator, vision_judge, edge_hunter, flake_analyst) that runs them
against real web/android/ios surfaces, verifies against a human-owned corpus
oracle, and reports — report-only: agents propose, humans decide.

Layout: `api/` (Go control plane: plans, approvals, corpus, flakes, trends,
agents, report), `agentrunner/` (TS agent loop + roster registry),
`web/` (admin BFF UI), `runner/` (suite container image), `registry/`
(config-as-code app registrations).

## Onboard a second app (APPS-03)

A second app is yaml + config — zero code changes. One registry file per
`(workflow, environment)` pair:

```sh
cp registry/betweb.yaml.example registry/betweb.yaml
# fill in owner/repo/workflow/environment/display_title_pattern,
# plans_source, webhook_secret_ref, comment_marker
```

Field names mirror the `registered_apps` DDL one-to-one (see
`api/internal/app/service.go`, the `App` struct) and are validated at boot —
a malformed file is a hard error. Key fields:

| yaml key                | purpose                                                        |
| ----------------------- | -------------------------------------------------------------- |
| `owner`/`repo`          | the app repo the webhook fires on                              |
| `workflow_name`/`workflow_path` | the GitHub Actions workflow e2ai reconciles            |
| `environment`           | run environment (`runs.environment`)                           |
| `display_title_pattern` | run-title filter; re-validated server-side on ingest           |
| `health_url`            | optional post-deploy readiness probe                           |
| `surfaces`              | `web` \| `android` \| `ios` — locked with the TS driver seam   |
| `plans_source`          | `repo_path` (dir in the app repo) or `central_repo` (+ `repo`) |
| `webhook_secret_ref`    | 1Password `op://` ref — per-app ingest secret (SECR-01)        |
| `comment_marker`        | sticky PR-comment anchor, unique per app (REPT-02)             |

After the yaml lands, two things stay DB-only per app (set via SQL on
`registered_apps`; no env vars, no code):

- edge-sweep opt-in (EDGE-01): `edge_sweep_enabled`,
  `edge_sweep_interval_minutes`, `edge_sweep_step_ceiling`
- flake alerting (FLAK-01): `flake_alert_threshold`

On boot the api seeds `registry/*.yaml` into `registered_apps` idempotently
(keyed by owner/repo/workflow_name/environment). Unknown-app webhook traffic
lands in `webhook_triage` for admin review, never a crash.
