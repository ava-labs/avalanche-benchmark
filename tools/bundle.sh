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

STAGE="$(mktemp -d "${TMPDIR:-/tmp}/bundle-XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT
NAME="$(basename "$OUT" .zip)"
mkdir -p "$STAGE/$NAME"

# Base layer: binaries, runtime configs, docs, monitoring.
cp -R bin "$STAGE/$NAME/bin"
cp README.md nodes.ini nodes.ini.example node-config.json \
   genesis-template.json subnet-config.json \
   chain-config.json chain-config-rpc.json chain-config-archive.json \
   "$STAGE/$NAME/"
for optional in oracle-genesis-template.json subnet-config-oracle.json \
                CONSENSUS-TUNING.md; do
  test -f "$optional" && cp "$optional" "$STAGE/$NAME/"
done
cp -R playbooks "$STAGE/$NAME/playbooks"
mkdir -p "$STAGE/$NAME/monitoring"
cp monitoring/grafana-datasources.yml "$STAGE/$NAME/monitoring/"
cp -R monitoring/dashboards "$STAGE/$NAME/monitoring/dashboards"
test -f monitoring/fleet-weight-exporter.py && \
  cp monitoring/fleet-weight-exporter.py "$STAGE/$NAME/monitoring/"

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
