#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"

# RPC nodes are on port 9654

if [ ! -f "$NETWORK_ENV" ]; then
    echo "ERROR: network.env not found. Run 02_create_l1.sh first."
    exit 1
fi

source "$NETWORK_ENV"

if [ -z "$CHAIN_ID" ]; then
    echo "ERROR: CHAIN_ID not found in network.env"
    exit 1
fi

# Build RPC URL (using first node's RPC port)
RPC_URL="http://$BOOTSTRAP_IP:9654/ext/bc/$CHAIN_ID/rpc"

echo "=== Benchmark ==="
echo "Chain ID: $CHAIN_ID"
echo ""
echo "RPC URL: $RPC_URL"
echo ""

exec "$SCRIPT_DIR/bin/bombard" --rpc "$RPC_URL" "$@"
