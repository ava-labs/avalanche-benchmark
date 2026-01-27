#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"

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

echo "=== Multi-Node Cleanup ==="
echo "Node 1: $NODE1_IP"
echo "Node 2: $NODE2_IP"
echo "Node 3: $NODE3_IP"
echo ""

cleanup_node() {
    local NODE_IP=$1
    local NODE_NAME=$2

    echo "Cleaning up $NODE_NAME ($NODE_IP)..."

    ssh "$NODE_IP" bash << 'CLEANUP_EOF'
# Kill avalanchego processes
pkill -f avalanchego 2>/dev/null || true

# Kill prometheus if running
pkill -f prometheus 2>/dev/null || true

# Kill grafana if running
pkill -f grafana 2>/dev/null || true

# Remove deployment directory
rm -rf ~/avalanche-benchmark

echo "  Done"
CLEANUP_EOF
}

cleanup_node "$NODE1_IP" "node1"
cleanup_node "$NODE2_IP" "node2"
cleanup_node "$NODE3_IP" "node3"

# Remove local state files
rm -f "$SCRIPT_DIR/network.env"
rm -f "$SCRIPT_DIR/prometheus.yml"

echo ""
echo "=== Cleanup Complete ==="
