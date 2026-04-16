#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"

echo "=== Multi-Node Cleanup ==="
print_nodes
echo ""

cleanup_node() {
    local NODE_IP=$1
    local NODE_NAME=$2

    echo "Cleaning up $NODE_NAME ($NODE_IP)..."

    ssh "$SSH_USER@$NODE_IP" bash << 'CLEANUP_EOF'
# Kill all avalanchego processes (primary, validator, rpc)
pkill -f "data-dir=data/primary" 2>/dev/null || true
pkill -f "data-dir=data/validator" 2>/dev/null || true
pkill -f "data-dir=data/rpc" 2>/dev/null || true

# Fallback: kill any remaining avalanchego
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

for i in "${!NODE_IPS_ARRAY[@]}"; do
    cleanup_node "${NODE_IPS_ARRAY[$i]}" "node$((i + 1))"
done

# Remove local state files
rm -f "$SCRIPT_DIR/network.env"
rm -f "$SCRIPT_DIR/prometheus.yml"
rm -f "$SCRIPT_DIR/grafana-dashboards.yml"

echo ""
echo "=== Cleanup Complete ==="
