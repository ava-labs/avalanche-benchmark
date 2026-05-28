#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"

# In sybil-enabled mode, benchmark validator RPC directly on port 9652.
# In the old sybil-disabled mode, dedicated RPC nodes are on port 9654.

if [ ! -f "$NETWORK_ENV" ]; then
    echo "ERROR: network.env not found. Run 02_create_l1.sh first."
    exit 1
fi

source "$NETWORK_ENV"

if [ -z "$CHAIN_ID" ]; then
    echo "ERROR: CHAIN_ID not found in network.env"
    exit 1
fi

RPC_PORT=9654
if is_truthy "$SYBIL_ENABLED_LOCAL"; then
    RPC_PORT=9652
fi

# Build RPC URL (using first node's benchmark port)
RPC_URL="http://$BOOTSTRAP_IP:$RPC_PORT/ext/bc/$CHAIN_ID/rpc"

echo "=== Benchmark ==="
echo "Chain ID: $CHAIN_ID"
echo ""
echo "RPC URL: $RPC_URL"
echo ""

exec "$SCRIPT_DIR/bin/bombard" --rpc "$RPC_URL" "$@"
