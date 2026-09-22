#!/usr/bin/env bash
# Merge the env-file layers exactly like the old Tiltfile did (base -> $ENV ->
# per-user, later wins) and emit a single Kubernetes Secret on stdout. Tilt feeds
# this into the cluster and pods pull it via `envFrom`, so the app sees the same
# resolved environment it saw when run as a bare process.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NAME="${SECRET_NAME:-e2ai-env}"
ENV="${ENV:-}"

# Layers are optional now — secretspec owns the base environment (declared in
# secretspec.toml); the files only carry overrides (staging overlay, per-user).
layers=()
if [ -f "$ROOT/.env" ]; then layers+=("$ROOT/.env"); fi
if [ -n "$ENV" ] && [ -f "$ROOT/.env.$ENV" ]; then layers+=("$ROOT/.env.$ENV"); fi
# Per-user overlay (idEntity per machine — EMAIL, DEV_FALLBACK_USER). Tilt runs are
# always local, so the overlay applies in every mode; ENV named runs (staging) just
# get the extra .env.$ENV layer in between.
if [ -f "$ROOT/.env.$(whoami).local" ]; then layers+=("$ROOT/.env.$(whoami).local"); fi

# Source in order so bash resolves quoting + later-layer overrides, just like
# `set -a; . .env; . .env.$ENV; ...` did before.
set -a
for f in "${layers[@]}"; do . "$f"; done
set +a

# Keys are whatever the layers declare (union). Read their *resolved* values.
keys=""
if [ "${#layers[@]}" -gt 0 ]; then
  keys=$(grep -hoE '^[A-Za-z_][A-Za-z0-9_]*=' "${layers[@]}" | sed 's/=$//' | sort -u)
fi

args=()
for k in $keys; do args+=(--from-literal="$k=${!k-}"); done

exec kubectl create secret generic "$NAME" \
  --namespace "${NAMESPACE:-e2ai}" \
  "${args[@]}" \
  --dry-run=client -o yaml
