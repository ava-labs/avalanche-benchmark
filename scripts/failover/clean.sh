#!/bin/bash
# Wipe a pool machine's local chain data and bring it back CLEAN.
#
# Use when a node is stuck BOOTSTRAPPING / its subnet VM keeps FATAL-ing because
# its local chain DB diverged or corrupted — e.g. after a hard kill (SIGKILL)
# under heavy load left it on an orphaned fork. A plain restart just re-FATALs on
# the same bad data; this wipes the chain DB so it re-bootstraps from the network.
#
# It removes only that node's data dir (chain DB, logs, generated chain configs).
# It does NOT touch staking credentials (staking/active, staking/l1) or the uploaded
# binaries/configs, so the node keeps its identity and comes back as the same
# validator/spare. On a co-located test box (a repeated IP in NODE_IPS) only the
# targeted instance's data dir/process is wiped — its housemates are left running.
#
# Usage: ./clean.sh <machine number>
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
source "$SCRIPT_DIR/_failover_common.sh"

M="$1"
if [ -z "$M" ]; then
    echo "usage: $0 <machine number>" >&2
    exit 2
fi

# The reconcile binary owns the instance math (which process/port/data dir a machine
# number maps to, including co-located boxes), so the clean is a single delegated call.
exec "$RECONCILE_BIN" clean "$M"
