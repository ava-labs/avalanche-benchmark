#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"
REMOTE_DIR="~/avalanche-benchmark"

# ------------------------------------------------------------------------------
# Load configuration
# ------------------------------------------------------------------------------
if [ ! -f "$ENV_FILE" ]; then
    echo "ERROR: .env file not found"
    echo ""
    echo "Create .env with your node IPs:"
    echo "  cp .env.example .env"
    echo "  # Edit .env and fill in NODE1_IP, NODE2_IP, NODE3_IP"
    exit 1
fi

source "$ENV_FILE"

if [ -z "$NODE1_IP" ] || [ -z "$NODE2_IP" ] || [ -z "$NODE3_IP" ]; then
    echo "ERROR: Missing node IPs in .env"
    echo ""
    echo "Required variables:"
    echo "  NODE1_IP=$NODE1_IP"
    echo "  NODE2_IP=$NODE2_IP"
    echo "  NODE3_IP=$NODE3_IP"
    exit 1
fi

echo "=== Multi-Node Primary Network Bootstrap ==="
echo "Node 1 (bootstrap): $NODE1_IP"
echo "Node 2 (validator): $NODE2_IP"
echo "Node 3 (validator): $NODE3_IP"
echo ""

# ------------------------------------------------------------------------------
# Step 1: Upload files to all nodes
# ------------------------------------------------------------------------------
echo "[1/4] Uploading files to all nodes..."

SUBNET_EVM_ID="srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"

for NODE_IP in $NODE1_IP $NODE2_IP $NODE3_IP; do
    echo "  Uploading to $NODE_IP..."
    ssh "$NODE_IP" "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR/bin $REMOTE_DIR/plugins"
    scp -q "$SCRIPT_DIR/bin/avalanchego" "$NODE_IP:$REMOTE_DIR/bin/"
    scp -q "$SCRIPT_DIR/bin/$SUBNET_EVM_ID" "$NODE_IP:$REMOTE_DIR/plugins/"
    scp -q "$SCRIPT_DIR/node-config.json" "$NODE_IP:$REMOTE_DIR/"
    scp -q "$SCRIPT_DIR/chain-config.json" "$NODE_IP:$REMOTE_DIR/"
done

echo "  Upload complete."

# ------------------------------------------------------------------------------
# Step 2: Start bootstrap node (node1)
# ------------------------------------------------------------------------------
echo "[2/4] Starting bootstrap node on $NODE1_IP..."

ssh "$NODE1_IP" bash << 'BOOTSTRAP_EOF'
set -e
cd ~/avalanche-benchmark

pkill -f avalanchego || true
sleep 1

rm -rf data
mkdir -p data/{db,logs}

nohup ./bin/avalanchego \
    --http-port=9650 \
    --staking-port=9651 \
    --http-host=0.0.0.0 \
    --db-dir=data/db \
    --log-dir=data/logs \
    --data-dir=data \
    --network-id=local \
    --sybil-protection-enabled=false \
    --plugin-dir="$(pwd)/plugins" \
    --config-file=node-config.json \
    --bootstrap-ips= \
    --bootstrap-ids= \
    >/dev/null 2>&1 &

echo $! > data/pid
BOOTSTRAP_EOF

echo "  Waiting for bootstrap node ID..."

BOOTSTRAP_NODE_ID=""
for i in {1..60}; do
    RESULT=$(curl -s -X POST --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' -H 'Content-Type: application/json' "http://$NODE1_IP:9650/ext/info" 2>/dev/null || true)
    BOOTSTRAP_NODE_ID=$(echo "$RESULT" | grep -o '"nodeID":"[^"]*"' | cut -d'"' -f4 || true)
    if [ -n "$BOOTSTRAP_NODE_ID" ]; then
        break
    fi
    sleep 1
done

if [ -z "$BOOTSTRAP_NODE_ID" ]; then
    echo "ERROR: Could not get bootstrap node ID"
    exit 1
fi

echo "  Bootstrap node ID: $BOOTSTRAP_NODE_ID"

# ------------------------------------------------------------------------------
# Step 3: Start validator nodes (node2, node3)
# ------------------------------------------------------------------------------
echo "[3/4] Starting validator nodes..."

start_validator_node() {
    local NODE_IP=$1
    local NODE_NUM=$2

    echo "  Starting node $NODE_NUM on $NODE_IP..."

    ssh "$NODE_IP" bash -s "$BOOTSTRAP_NODE_ID" "$NODE1_IP" << 'VALIDATOR_EOF'
set -e
BOOTSTRAP_NODE_ID=$1
BOOTSTRAP_IP=$2

cd ~/avalanche-benchmark

pkill -f avalanchego || true
sleep 1

rm -rf data
mkdir -p data/{db,logs}

nohup ./bin/avalanchego \
    --http-port=9650 \
    --staking-port=9651 \
    --http-host=0.0.0.0 \
    --db-dir=data/db \
    --log-dir=data/logs \
    --data-dir=data \
    --network-id=local \
    --sybil-protection-enabled=false \
    --plugin-dir="$(pwd)/plugins" \
    --config-file=node-config.json \
    --bootstrap-ips=${BOOTSTRAP_IP}:9651 \
    --bootstrap-ids=${BOOTSTRAP_NODE_ID} \
    >/dev/null 2>&1 &

echo $! > data/pid
VALIDATOR_EOF
}

start_validator_node "$NODE2_IP" 2
start_validator_node "$NODE3_IP" 3

# ------------------------------------------------------------------------------
# Step 4: Wait for all nodes to be healthy
# ------------------------------------------------------------------------------
echo "[4/4] Waiting for all nodes to be healthy..."

check_node_health() {
    local NODE_IP=$1

    for i in {1..60}; do
        if curl -sf "http://$NODE_IP:9650/ext/health" >/dev/null 2>&1; then
            RESULT=$(curl -s -X POST --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' -H 'Content-Type: application/json' "http://$NODE_IP:9650/ext/info" || true)
            NODE_ID=$(echo "$RESULT" | grep -o '"nodeID":"[^"]*"' | cut -d'"' -f4 || true)
            echo "$NODE_ID"
            return 0
        fi
        sleep 1
    done
    return 1
}

NODE2_ID=$(check_node_health "$NODE2_IP")
if [ -z "$NODE2_ID" ]; then
    echo "ERROR: Node 2 failed to start"
    exit 1
fi
echo "  Node 2 healthy: $NODE2_ID"

NODE3_ID=$(check_node_health "$NODE3_IP")
if [ -z "$NODE3_ID" ]; then
    echo "ERROR: Node 3 failed to start"
    exit 1
fi
echo "  Node 3 healthy: $NODE3_ID"

echo ""
echo "=== Primary Network Bootstrap Complete ==="
echo ""
echo "Node 1: $NODE1_IP - $BOOTSTRAP_NODE_ID (bootstrap)"
echo "Node 2: $NODE2_IP - $NODE2_ID"
echo "Node 3: $NODE3_IP - $NODE3_ID"
echo ""
echo "Next step: ./02_create_l1.sh"
