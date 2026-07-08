#!/bin/bash
# WIPE AND DEPLOY the L1 pool against the chain created (once) by setup/02_create_chain.sh. This is
# the repeatable from-scratch raise: DESTRUCTIVE BY DESIGN. It kills every pool
# node, WIPES data/ on all of them, and restarts the L1 from genesis (block 0),
# throwing away any current chain state. Losing the chain data is the design,
# not a recovery path. It never touches Fuji's P-chain, so re-deploys never
# re-spend on chain creation (the subnet/chain/validator registration from 02
# persists on Fuji). It then force-re-uploads binary/plugin/configs/keys,
# reseeds the intentions to the default mapping (validator keys 6..5+NVal on
# site A's validator slots, pinned home identities everywhere else), and starts
# all nodes (validators + hot spare + pinned dedicated-RPC trackers). After
# this, drive the fleet with ./fleet {up,down,mark,status}.
#
# First boot on a fresh fleet: the RPC tier full-replays Fuji's P-chain
# (~minutes) and the validators idle until their RPC beacons finish, then sync
# through them (serial per hop). Watch ./fleet status --watch, don't panic.
#
# For an in-place failover (no wipe) use ./fleet. A brand-new chain
# means re-running setup/02_create_chain.sh first (costs AVAX; usually you don't want that).
set -e
trap 'echo "ERROR: deploy.sh failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"
if [ ! -f "$SCRIPT_DIR/network.env" ]; then
    echo "ERROR: network.env not found. Run ./setup/02_create_chain.sh first." >&2
    exit 1
fi
source "$SCRIPT_DIR/network.env"

echo "=== Wipe and deploy (benchmark-fleet fresh) ==="
echo "Subnet ID: $SUBNET_ID"
echo "Chain ID:  $CHAIN_ID"
echo "This WIPES all L1 chain data on the pool and restarts the L1 from genesis."
echo "It does NOT re-create the chain on Fuji (no AVAX is spent)."
echo ""

"$SCRIPT_DIR/fleet" fresh

echo ""
IFS=',' read -ra _ips <<< "$NODE_IPS"
echo "Bombard ingress: the dedicated RPC node m5 (key 10, never promoted):"
echo "  http://${_ips[4]}:9652/ext/bc/$CHAIN_ID/rpc"
echo ""
echo "Validator RPC endpoints (m1-m3) and hot spare (m4) for reference:"
for i in 0 1 2 3; do
    echo "  http://${_ips[$i]}:9652/ext/bc/$CHAIN_ID/rpc"
done
echo ""
echo "Next: ./bombard.sh"
