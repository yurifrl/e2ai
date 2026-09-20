# runner — suite container image

The `e2ai-runner` process (api/cmd/e2ai-runner) drains the `runner` River
queue and, per execute job, shells out one `docker run` of **this image**.
One container = one suite execution at one commit.

## Contract

The suite.Executor (`api/internal/suite/execute.go`) runs the container with:

| env                  | meaning                                            |
| -------------------- | -------------------------------------------------- |
| `E2AI_REPO`          | `owner/repo` under test                            |
| `E2AI_SHA`           | exact head SHA the pipeline saw (SUIT-01)          |
| `E2AI_ARTIFACTS_DIR` | host artifacts dir bind-mounted at `/artifacts`    |
| `E2AI_GITHUB_TOKEN`  | PAT for private clones (omitted when unset)        |

On completion — whether the suite passed, failed, or errored — the
entrypoint must leave in `$E2AI_ARTIFACTS_DIR`:

- `results.json` — Playwright JSON reporter output (the parse input)
- everything from Playwright's output dir (screenshots/traces/videos)

Verdicts are derived by the runner process **only** from that evidence;
container exit codes are never consulted (VERD-01).

## Build

```sh
docker build -t e2ai-runner:local runner/
```

The runner process picks the image up via `RUNNER_IMAGE` (default
`e2ai-runner:local`) and the artifacts root via `RUNNER_ARTIFACTS_DIR`.

## Entrypoint notes

`entrypoint.sh` does: blobless clone → fetch/checkout `$E2AI_SHA` →
`corepack prepare` + `yarn install --immutable` (package manager and
versions come from the checkout, never from the image — RESEARCH.md
Pitfall 4) → `yarn playwright test --reporter=json` with
`PLAYWRIGHT_JSON_OUTPUT_NAME` pointed at the artifacts volume → stage
`test-results/` into the artifacts dir.

Bet-client layout assumptions (revisit if the app moves things):

- Playwright config lives at the repo root of the checkout and its
  `webServer` builds + serves the app on :3000 itself; the entrypoint does
  not start any server.
- `yarn` is the package manager; `yarn playwright test` resolves the
  checkout's own `@playwright/test` (version-matched to this image's base).

The base image tag (`mcr.microsoft.com/playwright:v1.58.2-noble`) is pinned
to the app's `@playwright/test` version — bump both together, and keep
`api/internal/suite/fixturegen/package.json` (the results.json fixture
generator) on the same version.
