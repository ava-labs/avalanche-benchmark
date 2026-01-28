#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"
NETWORK_ENV="$SCRIPT_DIR/network.env"

# RPC nodes are on port 9654

# ------------------------------------------------------------------------------
# Load configuration
# ------------------------------------------------------------------------------
if [ ! -f "$ENV_FILE" ]; then
    echo "ERROR: .env file not found"
    exit 1
fi

source "$ENV_FILE"

if [ -z "$NODE1_IP" ] || [ -z "$NODE2_IP" ] || [ -z "$NODE3_IP" ]; then
    echo "ERROR: Missing node IPs in .env"
    exit 1
fi

if [ ! -f "$NETWORK_ENV" ]; then
    echo "ERROR: network.env not found. Run 02_create_l1.sh first."
    exit 1
fi

source "$NETWORK_ENV"

if [ -z "$CHAIN_ID" ]; then
    echo "ERROR: CHAIN_ID not found in network.env"
    exit 1
fi

# Build RPC URLs (using RPC nodes on port 9654)
RPC1="http://$NODE1_IP:9654/ext/bc/$CHAIN_ID/rpc"
RPC2="http://$NODE2_IP:9654/ext/bc/$CHAIN_ID/rpc"
RPC3="http://$NODE3_IP:9654/ext/bc/$CHAIN_ID/rpc"
RPC_URLS="$RPC1,$RPC2,$RPC3"

echo "=== Benchmark ==="
echo "Chain ID: $CHAIN_ID"
echo ""
echo "RPC URLs (dedicated RPC nodes on port 9654):"
echo "  $RPC1"
echo "  $RPC2"
echo "  $RPC3"
echo ""

# Pass through any additional flags to bombard
exec "$SCRIPT_DIR/bin/bombard" -rpc "$RPC_URLS" "$@"
