#!/usr/bin/env bash
# Rebuild the kit and replace it on the control host.
#
# Only the `make pack` artifact is shipped. Go sources never land on control.
# The control workspace is wiped except for state that cannot be regenerated:
# the fleet inventory, the funding key, the generated identities, and the
# P-chain archive.
#
# Usage:
#   scripts/redeploy-control.sh                 full pack, rebuilds avalanchego
#   FAST=1 scripts/redeploy-control.sh          reuse bin/avalanchego, kit only
#   CONTROL=1.2.3.4 scripts/redeploy-control.sh override the control host
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

CONTROL="${CONTROL:-$(awk '$3 == "role=pchain" {sub(/^host=/, "", $2); print $2; exit}' nodes.ini)}"
if [ -z "$CONTROL" ]; then
  echo "no control host: pass CONTROL=<ip> or give nodes.ini a role=pchain row" >&2
  exit 1
fi
SSH_USER="${SSH_USER:-ubuntu}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/fleet}"
REMOTE_DIR="${REMOTE_DIR:-avalanche-benchmark}"

# Regenerated on every deploy, so never preserved. Everything not listed here
# is deleted from the control workspace.
KEEP=(.env nodes.ini deployment pchain.tar.gz)

SSH_OPTS=(-i "$SSH_KEY" -o IdentitiesOnly=yes -o IdentityAgent=none
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
  -o LogLevel=ERROR -o ConnectTimeout=15)
target="$SSH_USER@$CONTROL"

echo "==> building ${FAST:+pack-fast}${FAST:-pack}"
if [ -n "${FAST:-}" ]; then
  make pack-fast >/dev/null
else
  make pack >/dev/null
fi
ls -lh remote-benchmark.tar.gz

# Build the find(1) prune expression from KEEP so the wipe list stays in one
# place. Anything added to KEEP is preserved without touching the command.
prune=""
for keep in "${KEEP[@]}"; do
  prune="$prune -name $keep -o"
done
prune="${prune% -o}"

echo "==> wiping $target:~/$REMOTE_DIR (keeping: ${KEEP[*]})"
# shellcheck disable=SC2029
ssh -n "${SSH_OPTS[@]}" "$target" "
  set -eu
  mkdir -p ~/$REMOTE_DIR
  cd ~/$REMOTE_DIR
  find . -mindepth 1 -maxdepth 1 \\( $prune \\) -prune -o -exec rm -rf {} +
  ls -A
"

echo "==> shipping the pack artifact"
scp "${SSH_OPTS[@]}" remote-benchmark.tar.gz "$target:~/$REMOTE_DIR/remote-benchmark.tar.gz"
# shellcheck disable=SC2029
ssh -n "${SSH_OPTS[@]}" "$target" "
  set -eu
  cd ~/$REMOTE_DIR
  tar -xzf remote-benchmark.tar.gz
  rm -f remote-benchmark.tar.gz
  chmod 600 .env 2>/dev/null || true
  echo '--- control workspace ---'
  ls -A
  echo '--- versions ---'
  cat bin/VERSIONS
"

echo "==> done: ssh -i $SSH_KEY $target, then cd ~/$REMOTE_DIR"
