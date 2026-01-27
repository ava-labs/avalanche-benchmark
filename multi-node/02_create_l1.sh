#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"
NETWORK_ENV="$SCRIPT_DIR/network.env"
REMOTE_DIR="~/avalanche-benchmark"

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

# ------------------------------------------------------------------------------
# Step 1: Create L1 (subnet + chain + convert)
# ------------------------------------------------------------------------------
"$SCRIPT_DIR/bin/create-l1" -output "$NETWORK_ENV"

# Load the results
source "$NETWORK_ENV"

echo ""
echo "[5/5] Restarting nodes with --track-subnets..."

# ------------------------------------------------------------------------------
# Step 2: Restart nodes with --track-subnets
# ------------------------------------------------------------------------------
restart_node() {
    local NODE_IP=$1
    local NODE_NUM=$2
    local BOOTSTRAP_ID=$3
    local BOOTSTRAP_IP=$4

    echo "  Restarting node $NODE_NUM on $NODE_IP..."

    ssh "$NODE_IP" bash -s "$SUBNET_ID" "$CHAIN_ID" "$BOOTSTRAP_ID" "$BOOTSTRAP_IP" << 'RESTART_EOF'
set -e
SUBNET_ID=$1
CHAIN_ID=$2
BOOTSTRAP_ID=$3
BOOTSTRAP_IP=$4

cd ~/avalanche-benchmark

# Kill existing
pkill -f avalanchego || true
sleep 2

# Install chain config
mkdir -p "data/configs/chains/$CHAIN_ID"
cp chain-config.json "data/configs/chains/$CHAIN_ID/config.json"

# Build args
ARGS=(
    --http-port=9650
    --staking-port=9651
    --http-host=0.0.0.0
    --db-dir=data/db
    --log-dir=data/logs
    --data-dir=data
    --network-id=local
    --sybil-protection-enabled=false
    --plugin-dir="$(pwd)/plugins"
    --config-file=node-config.json
    --chain-config-dir=data/configs/chains
    --track-subnets="$SUBNET_ID"
)

# Add bootstrap flags if not bootstrap node
if [ -n "$BOOTSTRAP_ID" ]; then
    ARGS+=(--bootstrap-ips="${BOOTSTRAP_IP}:9651")
    ARGS+=(--bootstrap-ids="$BOOTSTRAP_ID")
else
    ARGS+=(--bootstrap-ips=)
    ARGS+=(--bootstrap-ids=)
fi

nohup ./bin/avalanchego "${ARGS[@]}" >/dev/null 2>&1 &

echo $! > data/pid
RESTART_EOF
}

# Get bootstrap node ID first
BOOTSTRAP_NODE_ID=$(curl -s -X POST --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' \
    -H 'Content-Type: application/json' "http://$NODE1_IP:9650/ext/info" | \
    grep -o '"nodeID":"[^"]*"' | cut -d'"' -f4)

# Restart node 1 (bootstrap - no bootstrap flags)
restart_node "$NODE1_IP" 1 "" ""

# Wait for node 1 ID
echo "  Waiting for node 1 ID..."
BOOTSTRAP_NODE_ID=""
for i in {1..60}; do
    RESULT=$(curl -s -X POST --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' \
        -H 'Content-Type: application/json' "http://$NODE1_IP:9650/ext/info" 2>/dev/null || true)
    BOOTSTRAP_NODE_ID=$(echo "$RESULT" | grep -o '"nodeID":"[^"]*"' | cut -d'"' -f4 || true)
    if [ -n "$BOOTSTRAP_NODE_ID" ]; then
        break
    fi
    sleep 1
done

# Restart nodes 2 and 3
restart_node "$NODE2_IP" 2 "$BOOTSTRAP_NODE_ID" "$NODE1_IP"
restart_node "$NODE3_IP" 3 "$BOOTSTRAP_NODE_ID" "$NODE1_IP"

# Wait for all nodes
echo "  Waiting for all nodes to be healthy..."
for NODE_IP in $NODE1_IP $NODE2_IP $NODE3_IP; do
    for i in {1..60}; do
        if curl -sf "http://$NODE_IP:9650/ext/health" >/dev/null 2>&1; then
            break
        fi
        sleep 1
    done
done

echo ""
echo "=== L1 Ready ==="
echo ""
echo "Subnet ID: $SUBNET_ID"
echo "Chain ID:  $CHAIN_ID"
echo ""
echo "RPC Endpoints:"
echo "  Node 1: http://$NODE1_IP:9650/ext/bc/$CHAIN_ID/rpc"
echo "  Node 2: http://$NODE2_IP:9650/ext/bc/$CHAIN_ID/rpc"
echo "  Node 3: http://$NODE3_IP:9650/ext/bc/$CHAIN_ID/rpc"
