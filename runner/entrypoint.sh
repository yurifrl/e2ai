#!/bin/sh
# Suite container entrypoint (SUIT-01/02). Invoked by suite.Executor's docker
# run with:
#
#   E2AI_REPO          owner/repo of the app under test
#   E2AI_SHA           head SHA to test (the exact commit the pipeline saw)
#   E2AI_ARTIFACTS_DIR output dir, bind-mounted from the host (/artifacts)
#   E2AI_GITHUB_TOKEN  optional PAT for private checkouts
#
# Contract with the executor: on success OR failure of the suite itself,
# results.json (Playwright JSON reporter) plus everything from Playwright's
# output dir (screenshots, traces, videos) end up in $E2AI_ARTIFACTS_DIR.
# The runner process parses results.json and derives verdicts from that
# evidence — the container's exit code is never a verdict (VERD-01).
set -eu

repo="${E2AI_REPO:?E2AI_REPO is required}"
sha="${E2AI_SHA:?E2AI_SHA is required}"
out="${E2AI_ARTIFACTS_DIR:?E2AI_ARTIFACTS_DIR is required}"

cd /work

if [ -n "${E2AI_GITHUB_TOKEN:-}" ]; then
  # Header-auth clone: the token never appears in remote URLs (which git
  # persists in .git/config and leaks through error messages).
  git -c "http.extraheader=AUTHORIZATION: basic $(printf 'x-access-token:%s' "$E2AI_GITHUB_TOKEN" | base64)" \
    clone --filter=blob:none "https://github.com/${repo}.git" app
else
  git clone --filter=blob:none "https://github.com/${repo}.git" app
fi
cd app
# Full object graph (blobless clone keeps fetches fast); the SHA is the
# pipeline's identity, not a branch that may have moved since.
git fetch --filter=blob:none origin "$sha"
git checkout --detach "$sha"

# Package manager + versions come from the checkout itself (corepack reads
# packageManager; the lockfile pins every dependency). Nothing here is
# hardcoded to bet-client beyond the layout assumption documented in
# runner/README.md.
corepack prepare
yarn install --immutable

# playwright's webServer config builds and serves the app on :3000 itself;
# the entrypoint only runs the suite with the JSON reporter forced on and
# pointed at the artifacts volume.
PLAYWRIGHT_JSON_OUTPUT_NAME="${out}/results.json" \
  yarn playwright test --reporter=json || true

# Stage Playwright's evidence next to results.json (screenshots, traces,
# videos live under test-results/ by default). The runner process indexes
# every file in $out as an artifact row.
if [ -d test-results ]; then
  cp -a test-results/. "$out/"
fi
