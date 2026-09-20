# Runner image for the e2ai suite container (T8). The api/runner process
# builds this once and `docker run`s one instance per execute job with the
# run's artifacts directory mounted at /artifacts (suite.Executor).
#
# Base is pinned to bet-client's @playwright/test version — browsers ship in
# the image, and a version drift with the checkout's lockfile is the #1 way
# this suite silently breaks (RESEARCH.md Pitfall 4). Bump both together.
# fixturegen (api/internal/suite/fixturegen) pins the same version.
FROM mcr.microsoft.com/playwright:v1.58.2-noble

# The entrypoint clones the app checkout at the run's SHA; git + ca certs
# are the only things the playwright base lacks. corepack is preinstalled
# with node but disabled by default.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && corepack enable

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Suite containers are ephemeral and unprivileged: everything happens in
# /work (clone, install, build, test) and outputs go only to /artifacts.
ENTRYPOINT ["/entrypoint.sh"]
