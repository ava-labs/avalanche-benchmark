#!/bin/bash
# WIPE AND DEPLOY the L1 pool against the chain created by 01/02.
# DESTRUCTIVE: this kills every pool node, WIPES data/ on all of them, and
# restarts the L1 from genesis (block 0) — any current chain state is thrown
# away. It then force-re-uploads binary/plugin/configs/keys, reseeds the
# intentions to the default mapping (every data center's validator slots host
# live validators — active-active), and starts every node. After this, use
# scripts/failover/{up,down,failover,status}.sh.
#
# A brand-new chain means re-running 01 then 02 first.
set -e
trap 'echo "ERROR: 03 failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$SCRIPT_DIR"
source "$SCRIPT_DIR/scripts/failover/_failover_common.sh"

echo "=== Wipe and deploy (reconcile --fresh) ==="
echo "Subnet ID: $SUBNET_ID"
echo "Chain ID:  $CHAIN_ID"
echo "This WIPES all chain data on every pool machine and restarts the L1 from genesis."
echo ""
echo "Node → data center map:"
"$RECONCILE_BIN" endpoints | awk -F'\t' '{printf "  %-3s  DC-%s  %-5s  %s:%s\n", $1, toupper($2), $3, $4, $5}'
echo ""

"$RECONCILE_BIN" fresh

echo ""
echo "Bombard ingress — the pinned RPC trackers (never validators), per DC:"
"$RECONCILE_BIN" endpoints | awk -F'\t' -v c="$CHAIN_ID" \
    '$3=="rpc"{printf "  DC-%s  http://%s:%s/ext/bc/%s/rpc\n", toupper($2), $4, $5, c}'
echo ""
echo "Next: ./05_benchmark.sh"
