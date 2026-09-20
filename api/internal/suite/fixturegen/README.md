# fixturegen — real Playwright results.json fixture

Generates `../testdata/results.json` (grounds parser enum table for T8 / assumption A2).

**Re-run:**

```sh
cd api/internal/suite/fixturegen
npm install          # pins @playwright/test 1.58.2 (bet-client's version)
npx playwright install chromium   # chromium only, ~160MB
PLAYWRIGHT_JSON_OUTPUT_NAME=../testdata/results.json npx playwright test --reporter=json
```

playwright.config.ts sets `retries: 1`, `workers: 1` (single project).

Observed fixture tests: `passes` (expected), `always fails` (unexpected+failed),
`flaky then passes` (flaky, 2 attempts), `skipped` (skipped via `test.fixme`).

Parser note (grounds T8's enum table): Playwright 1.58's JSON reporter names the
top-level test verdict field `status` (not `outcome`) with values
`expected` | `unexpected` | `flaky` | `skipped`; per-attempt entries live in
`results[]` with `status` `passed`/`failed`/`skipped`/`timedOut`/`interrupted`.
