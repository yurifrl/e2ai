#!/usr/bin/env bash
# Merge the env-file layers exactly like the old Tiltfile did (base -> $ENV ->
# per-user, later wins) and emit a single Kubernetes Secret on stdout. Tilt feeds
# this into the cluster and pods pull it via `envFrom`, so the app sees the same
# resolved environment it saw when run as a bare process.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NAME="${SECRET_NAME:-e2ai-env}"
ENV="${ENV:-}"

layers=("$ROOT/.env")
[ -n "$ENV" ] && [ -f "$ROOT/.env.$ENV" ] && layers+=("$ROOT/.env.$ENV")
# Per-user overlay (idEntity per machine — EMAIL, DEV_FALLBACK_USER). Tilt runs are
# always local, so the overlay applies in every mode; ENV named runs (staging) just
# get the extra .env.$ENV layer in between.
[ -f "$ROOT/.env.$(whoami).local" ] && layers+=("$ROOT/.env.$(whoami).local")

# Source in order so bash resolves quoting + later-layer overrides, just like
# `set -a; . .env; . .env.$ENV; ...` did before.
set -a
for f in "${layers[@]}"; do . "$f"; done
set +a

# Keys are whatever the layers declare (union). Read their *resolved* values.
keys=$(grep -hoE '^[A-Za-z_][A-Za-z0-9_]*=' "${layers[@]}" | sed 's/=$//' | sort -u)

args=()
for k in $keys; do args+=(--from-literal="$k=${!k-}"); done

exec kubectl create secret generic "$NAME" \
  --namespace "${NAMESPACE:-e2ai}" \
  "${args[@]}" \
  --dry-run=client -o yaml
