#!/bin/bash
set -e

# Multi-node primary network bootstrap script
# Usage: ./01_bootstrap_primary_network.sh <node1_ip> <node2_ip> <node3_ip>

if [ "$#" -ne 3 ]; then
    echo "Usage: $0 <node1_ip> <node2_ip> <node3_ip>"
    echo "  node1 = bootstrap node (also runs Prometheus + Grafana later)"
    echo "  node2 = validator"
    echo "  node3 = validator"
    exit 1
fi

NODE1_IP=$1
NODE2_IP=$2
NODE3_IP=$3

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REMOTE_DIR="/tmp/avalanche-benchmark"

echo "=== Multi-Node Primary Network Bootstrap ==="
echo "Node 1 (bootstrap): $NODE1_IP"
echo "Node 2 (validator): $NODE2_IP"
echo "Node 3 (validator): $NODE3_IP"
echo ""

# ------------------------------------------------------------------------------
# Step 1: Upload files to all nodes
# ------------------------------------------------------------------------------
echo "[1/4] Uploading files to all nodes..."

for NODE_IP in $NODE1_IP $NODE2_IP $NODE3_IP; do
    echo "  Uploading to $NODE_IP..."
    ssh "$NODE_IP" "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR/bin $REMOTE_DIR/plugins"
    scp -q "$SCRIPT_DIR/bin/avalanchego" "$NODE_IP:$REMOTE_DIR/bin/"
    scp -q "$SCRIPT_DIR/plugins/"* "$NODE_IP:$REMOTE_DIR/plugins/"
    scp -q "$SCRIPT_DIR/node-config.json" "$NODE_IP:$REMOTE_DIR/"
done

echo "  Upload complete."

# ------------------------------------------------------------------------------
# Step 2: Start bootstrap node (node1)
# ------------------------------------------------------------------------------
echo "[2/4] Starting bootstrap node on $NODE1_IP..."

ssh "$NODE1_IP" bash << 'BOOTSTRAP_EOF'
set -e
cd /tmp/avalanche-benchmark

pkill -f avalanchego || true
sleep 1

rm -rf data
mkdir -p data/node-1/{db,logs}

PLUGIN_DIR="$(pwd)/plugins"

nohup ./bin/avalanchego \
    --http-port=9650 \
    --staking-port=9651 \
    --http-host=0.0.0.0 \
    --db-dir=data/node-1/db \
    --log-dir=data/node-1/logs \
    --data-dir=data/node-1 \
    --network-id=local \
    --sybil-protection-enabled=false \
    --plugin-dir="$PLUGIN_DIR" \
    --config-file=node-config.json \
    --bootstrap-ips= \
    --bootstrap-ids= \
    > data/node-1/logs/stdout.log 2>&1 &

echo $! > data/node-1/pid
echo "Bootstrap node started with PID $(cat data/node-1/pid)"
BOOTSTRAP_EOF

echo "  Waiting for bootstrap node to be healthy..."

for i in {1..60}; do
    if ssh "$NODE1_IP" "curl -sf http://127.0.0.1:9650/ext/health >/dev/null 2>&1"; then
        break
    fi
    sleep 1
done

if ! ssh "$NODE1_IP" "curl -sf http://127.0.0.1:9650/ext/health >/dev/null 2>&1"; then
    echo "ERROR: Bootstrap node failed to become healthy"
    exit 1
fi

# Get NodeID
RESULT=$(ssh "$NODE1_IP" "curl -s -X POST --data '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"info.getNodeID\"}' -H 'Content-Type: application/json' http://127.0.0.1:9650/ext/info")
BOOTSTRAP_NODE_ID=$(echo "$RESULT" | grep -o '"nodeID":"[^"]*"' | cut -d'"' -f4)

if [ -z "$BOOTSTRAP_NODE_ID" ]; then
    echo "ERROR: Could not get bootstrap node ID"
    exit 1
fi

echo "  Bootstrap node healthy: $BOOTSTRAP_NODE_ID"

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

cd /tmp/avalanche-benchmark

pkill -f avalanchego || true
sleep 1

rm -rf data
mkdir -p data/node-1/{db,logs}

PLUGIN_DIR="$(pwd)/plugins"

nohup ./bin/avalanchego \
    --http-port=9650 \
    --staking-port=9651 \
    --http-host=0.0.0.0 \
    --db-dir=data/node-1/db \
    --log-dir=data/node-1/logs \
    --data-dir=data/node-1 \
    --network-id=local \
    --sybil-protection-enabled=false \
    --plugin-dir="$PLUGIN_DIR" \
    --config-file=node-config.json \
    --bootstrap-ips=${BOOTSTRAP_IP}:9651 \
    --bootstrap-ids=${BOOTSTRAP_NODE_ID} \
    > data/node-1/logs/stdout.log 2>&1 &

echo $! > data/node-1/pid
echo "Validator node started with PID $(cat data/node-1/pid)"
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
        if ssh "$NODE_IP" "curl -sf http://127.0.0.1:9650/ext/health >/dev/null 2>&1"; then
            # Get NodeID once healthy
            RESULT=$(ssh "$NODE_IP" "curl -s -X POST --data '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"info.getNodeID\"}' -H 'Content-Type: application/json' http://127.0.0.1:9650/ext/info")
            NODE_ID=$(echo "$RESULT" | grep -o '"nodeID":"[^"]*"' | cut -d'"' -f4)
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

# ------------------------------------------------------------------------------
# Save node info for next steps
# ------------------------------------------------------------------------------
cat > "$SCRIPT_DIR/network-info.env" << EOF
NODE1_IP=$NODE1_IP
NODE2_IP=$NODE2_IP
NODE3_IP=$NODE3_IP
NODE1_ID=$BOOTSTRAP_NODE_ID
NODE2_ID=$NODE2_ID
NODE3_ID=$NODE3_ID
BOOTSTRAP_IP=$NODE1_IP
BOOTSTRAP_ID=$BOOTSTRAP_NODE_ID
EOF

echo ""
echo "=== Primary Network Bootstrap Complete ==="
echo ""
echo "Node 1: $NODE1_IP - $BOOTSTRAP_NODE_ID (bootstrap)"
echo "Node 2: $NODE2_IP - $NODE2_ID"
echo "Node 3: $NODE3_IP - $NODE3_ID"
echo ""
echo "Network info saved to: $SCRIPT_DIR/network-info.env"
echo ""
echo "Next step: ./02_create_l1.sh"
