#!/bin/bash
set -e

# Multi-node cleanup script
# Usage: ./09_cleanup.sh <node1_ip> <node2_ip> <node3_ip>
#    or: ./09_cleanup.sh (reads from network-info.env)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Load IPs from env file or command line
if [ "$#" -eq 3 ]; then
    NODE1_IP=$1
    NODE2_IP=$2
    NODE3_IP=$3
elif [ -f "$SCRIPT_DIR/network-info.env" ]; then
    source "$SCRIPT_DIR/network-info.env"
else
    echo "Usage: $0 <node1_ip> <node2_ip> <node3_ip>"
    echo "   or: $0 (if network-info.env exists)"
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
rm -rf /tmp/avalanche-benchmark

echo "  Done"
CLEANUP_EOF
}

cleanup_node "$NODE1_IP" "node1"
cleanup_node "$NODE2_IP" "node2"
cleanup_node "$NODE3_IP" "node3"

# Clean local generated files
echo "Cleaning local files..."
rm -f "$SCRIPT_DIR/network-info.env"
rm -f "$SCRIPT_DIR/avalanche-deploy.tar.gz"

echo ""
echo "=== Cleanup Complete ==="
