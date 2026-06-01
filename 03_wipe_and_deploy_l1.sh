#!/bin/bash
# WIPE AND DEPLOY the L1 validator pool (machines 1-4) against the chain created
# by 01/02. DESTRUCTIVE: this kills every pool node, WIPES data/ on all of them,
# and restarts the L1 from genesis (block 0) — any current chain state is thrown
# away. It then force-re-uploads binary/plugin/configs/keys, reseeds the
# intentions to the default {m1:6, m2:7, m3:8, m4:9}, and starts all four nodes
# (3 validators + 1 hot spare). After this, use scripts/failover/{up,down,failover}.sh.
#
# For an in-place failover (no wipe) use scripts/failover/. A brand-new chain
# means re-running 01 then 02 first.
set -e
trap 'echo "ERROR: 03 failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$SCRIPT_DIR"
source "$SCRIPT_DIR/scripts/failover/_failover_common.sh"

echo "=== Wipe and deploy (reconcile --fresh) ==="
echo "Subnet ID: $SUBNET_ID"
echo "Chain ID:  $CHAIN_ID"
echo "Pool: machines 1-4 (3 validators staking/l1/6,7,8 + hot spare staking/l1/9)"
echo "This WIPES all chain data on machines 1-4 and restarts the L1 from genesis."
echo ""

"$RECONCILE_BIN" fresh

echo ""
echo "Validator/RPC endpoints (pass ALL to bombard):"
IFS=',' read -ra _ips <<< "$NODE_IPS"
for i in 0 1 2 3; do
    echo "  http://${_ips[$i]}:9652/ext/bc/$CHAIN_ID/rpc"
done
echo ""
echo "Next: ./05_benchmark.sh"
