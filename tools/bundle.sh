#!/usr/bin/env bash
# Cut a client bundle: base layer + exactly one app, from a live deployment
# root. Replaces the hand-staged zips; run via `make bundle APP=<name>`.
#
# A bundle is NOT `make pack`. pack is binaries+configs for shipping to a
# control machine; a bundle is a complete, deployable handover including the
# deployment's identities and the frozen P-chain archive, so it can only be
# cut where `l1 create` ran. Secrets are stripped from the shipped .env, and
# an optional .bundle-denylist (one pattern per line, gitignored) hard-fails
# the cut if any pattern appears anywhere in the staged tree.
set -euo pipefail

APP="${1:?usage: bundle.sh <app> <output.zip>}"
OUT="${2:?usage: bundle.sh <app> <output.zip>}"
ROOT="$(pwd)"

require() { test -e "$1" || { echo "bundle: missing required $1" >&2; exit 1; }; }
require "apps/$APP"
require bin/avalanchego
require bin/fleet
require .env
require nodes.ini
require deployment
require pchain.tar.gz
require chains/default

STAGE="$(mktemp -d "${TMPDIR:-/tmp}/bundle-XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT
NAME="$(basename "$OUT" .zip)"
mkdir -p "$STAGE/$NAME"

# Base layer: binaries, per-chain configuration, docs, monitoring. All
# runtime configs live under chains/ (chains/default/ holds the shared
# defaults); legacy root names from old deployment roots ride along when
# present so a re-cut of an old kit stays deployable.
cp -R bin "$STAGE/$NAME/bin"
cp README.md nodes.ini "$STAGE/$NAME/"
cp -R chains "$STAGE/$NAME/chains"
for legacy in genesis-template.json subnet-config.json node-config.json \
              chain-config.json chain-config-rpc.json chain-config-archive.json \
              oracle-genesis-template.json subnet-config-oracle.json; do
  test -f "$legacy" && cp "$legacy" "$STAGE/$NAME/"
done
cp -R playbooks "$STAGE/$NAME/playbooks"
cp -R examples "$STAGE/$NAME/examples"
# The docs tell the operator to run tools/forkcheck.sh after every load
# test, so the bundle must carry it. bundle.sh itself stays out: cutting
# bundles is not a client operation.
mkdir -p "$STAGE/$NAME/tools"
if test -f tools/forkcheck.sh; then
  cp tools/forkcheck.sh "$STAGE/$NAME/tools/"
else
  cp "$(dirname "$0")/forkcheck.sh" "$STAGE/$NAME/tools/"
fi
test -d docs && cp -R docs "$STAGE/$NAME/docs"
mkdir -p "$STAGE/$NAME/monitoring"
cp monitoring/grafana-datasources.yml monitoring/grafana-dashboards.yml \
   monitoring/prometheus.yml monitoring/docker-compose.yml \
   monitoring/alerts.yml \
   monitoring/fleet-weight-exporter.py "$STAGE/$NAME/monitoring/"
cp -R monitoring/dashboards "$STAGE/$NAME/monitoring/dashboards"

# The app: dashboards overlay, docs, runbook, contract sources for reference.
mkdir -p "$STAGE/$NAME/apps"
cp -R "apps/$APP" "$STAGE/$NAME/apps/$APP"
rm -rf "$STAGE/$NAME/apps/$APP/contracts/lib" \
       "$STAGE/$NAME/apps/$APP/contracts/cache" \
       "$STAGE/$NAME/apps/$APP/contracts/out"

# The deployment: identities, placement, network records, frozen P-chain.
cp -R deployment "$STAGE/$NAME/deployment"
cp pchain.tar.gz "$STAGE/$NAME/"

# Shipped .env: the deployment's real settings with every secret blanked.
# PCHAIN_API_TOKEN is a legacy field: the kit no longer reads it, but old
# deployment roots still carry it, so the sanitizer keeps blanking it.
sed -E 's/^(FUNDING_PRIVATE_KEY)=.*/\1=/; s/^(PCHAIN_API_TOKEN)=.*/\1=/' \
  .env > "$STAGE/$NAME/.env"
grep -q '^REMOTE_DIR=' "$STAGE/$NAME/.env" || {
  printf '\n# Install root. Empty means /home/<SSH_USER>/avalanche-benchmark (user-level, no root).\nREMOTE_DIR=\nREMOTE_DATA_DIR=\n' \
    >> "$STAGE/$NAME/.env"
}
if grep -qE '^(FUNDING_PRIVATE_KEY|PCHAIN_API_TOKEN)=..' "$STAGE/$NAME/.env"; then
  echo "bundle: a secret survived .env sanitization" >&2; exit 1
fi

# Deny gate: nothing on the denylist may appear anywhere in the bundle.
if test -s "$ROOT/.bundle-denylist"; then
  PATTERNS="$(paste -sd'|' "$ROOT/.bundle-denylist")"
  if grep -rIiEq "$PATTERNS" "$STAGE/$NAME" --exclude-dir=bin; then
    echo "bundle: denylist pattern found in staged tree:" >&2
    grep -rIiE "$PATTERNS" "$STAGE/$NAME" --exclude-dir=bin -l >&2
    exit 1
  fi
fi

(cd "$STAGE" && zip -qr "$ROOT/$OUT" "$NAME")
echo "bundle: wrote $OUT ($(du -h "$ROOT/$OUT" | cut -f1))"
echo "bundle: REMINDER: the zip contains live deployment keys; treat the"
echo "bundle: artifact and its download link as secrets."
