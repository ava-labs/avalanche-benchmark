#!/bin/bash
# Wipe a pool machine's local chain data and bring it back CLEAN.
#
# Use when a node is stuck BOOTSTRAPPING / its subnet VM keeps FATAL-ing because
# its local chain DB diverged or corrupted — e.g. after a hard kill (SIGKILL)
# under heavy load left it on an orphaned fork. A plain restart just re-FATALs on
# the same bad data; this wipes the chain DB so it re-bootstraps from the network.
#
# It removes only data/ (chain DB, logs, generated chain configs). It does NOT
# touch staking credentials (staking/active, staking/l1) or the uploaded
# binaries/configs, so the node keeps its identity and comes back as the same
# validator/spare.
#
# Usage: ./clean.sh <machine 1-4>
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
source "$SCRIPT_DIR/_failover_common.sh"

M="$1"
if [ -z "$M" ]; then
    echo "usage: $0 <machine 1-4>" >&2
    exit 2
fi

IP="${NODE_IPS_ARRAY[$((M - 1))]}"
if [ -z "$IP" ]; then
    echo "ERROR: no IP for machine $M (have ${#NODE_IPS_ARRAY[@]} nodes)" >&2
    exit 1
fi

echo "== clean machine $M ($IP): wipe chain data, keep credentials =="

# Kill avalanchego AND the subnet-evm plugin child (the [p] bracket stops the
# pattern from matching this shell), then wipe only the data/ tree.
ssh "$SSH_USER@$IP" "
    pkill -KILL -x avalanchego 2>/dev/null || true
    pkill -KILL -f 'avalanche-benchmark/[p]lugins/' 2>/dev/null || true
    sleep 1
    rm -rf $REMOTE_DIR/data
    echo \"  wiped $REMOTE_DIR/data (staking/ left intact)\"
"

# Bring it back up clean via the normal reconcile pass: the node is now down with
# its intent still 'up', so apply restarts it with its current key against a fresh
# DB. Other nodes are a no-op.
echo "== restarting clean via reconcile apply =="
exec "$RECONCILE_BIN" apply
