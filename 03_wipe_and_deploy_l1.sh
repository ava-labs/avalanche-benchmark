#!/bin/bash
# WIPE AND DEPLOY the L1 pool (machines 1-5) against the chain created by 01/02.
# DESTRUCTIVE: this kills every pool node, WIPES data/ on all of them, and
# restarts the L1 from genesis (block 0) — any current chain state is thrown
# away. It then force-re-uploads binary/plugin/configs/keys, reseeds the
# intentions to the default {m1:6, m2:7, m3:8, m4:9(spare), m5:10(rpc)}, and
# starts all five nodes (3 validators + 1 hot spare + 1 pinned dedicated-RPC
# tracker). After this, use scripts/failover/{up,down,failover}.sh.
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
echo "Pool: machines 1-5 (3 validators staking/l1/6,7,8 + hot spare staking/l1/9 + dedicated RPC staking/l1/10)"
echo "This WIPES all chain data on machines 1-5 and restarts the L1 from genesis."
echo ""

"$RECONCILE_BIN" fresh

echo ""
IFS=',' read -ra _ips <<< "$NODE_IPS"
echo "Bombard ingress — the dedicated RPC node m5 (key 10, never promoted):"
echo "  http://${_ips[4]}:9652/ext/bc/$CHAIN_ID/rpc"
echo ""
echo "Validator RPC endpoints (m1-m3) and hot spare (m4) for reference:"
for i in 0 1 2 3; do
    echo "  http://${_ips[$i]}:9652/ext/bc/$CHAIN_ID/rpc"
done
echo ""
echo "Next: ./05_benchmark.sh"
