#!/bin/bash
# WIPE AND DEPLOY the L1 pool against the chain created (once) by setup/02_create_chain.sh. This is
# the repeatable from-scratch raise: DESTRUCTIVE BY DESIGN. It kills every pool
# node, WIPES data/ on all of them, and restarts the L1 from genesis (block 0),
# throwing away any current chain state. Losing the chain data is the design,
# not a recovery path. It never touches Fuji's P-chain, so re-deploys never
# re-spend on chain creation (the subnet/chain/validator registration from 02
# persists on Fuji). It then force-re-uploads binary/plugin/configs/keys,
# reseeds the intentions to the default mapping (validator keys 1..NVal on
# site A's validator slots, pinned home identities everywhere else), and starts
# all nodes (validators + hot spare + pinned dedicated-RPC trackers). After
# this, drive the fleet with ./fleet {up,down,mark,status}.
#
# First boot on a fresh fleet: the RPC tier full-replays Fuji's P-chain
# (~minutes) and the validators idle until their RPC beacons finish, then sync
# through them (serial per hop). Watch with: watch -n5 ./fleet status, don't panic.
#
# For an in-place failover (no wipe) use ./fleet. A brand-new chain
# means re-running setup/02_create_chain.sh first (costs AVAX; usually you don't want that).
set -e
trap 'echo "ERROR: run/01_deploy.sh failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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
echo "On-chain weights were not touched (they persist on the P-chain). To reset"
echo "them to the healthy baseline: ./scenarios/00_healthy.sh, or bin/l1 apply."
echo ""
# Same role=rpc extraction as run/03_bombard.sh: co-location-aware ports, both sites.
# bombard fans every tx across ALL of these and rides through a site failover.
echo "Bombard ingress (all pinned RPC nodes, never promoted to validators):"
export NODE_IPS BACKUP_SITE_NODE_IPS
"$SCRIPT_DIR/bin/benchmark-fleet" endpoints | awk -F'\t' -v c="$CHAIN_ID" \
    '$3=="rpc"{printf "  http://%s:%s/ext/bc/%s/rpc\n", $4, $5, c}'
echo ""
echo "Next: ./run/03_bombard.sh"
