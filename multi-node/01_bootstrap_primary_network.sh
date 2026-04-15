#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"

# Port layout per machine:
#   Primary:   HTTP 9650, Staking 9651
#   Validator: HTTP 9652, Staking 9653
#   RPC:       HTTP 9654, Staking 9655

echo "=== Multi-Node Primary Network Bootstrap ==="
print_nodes
echo ""
echo "Starting PRIMARY NETWORK nodes only (port 9650/9651)"
echo ""

# ------------------------------------------------------------------------------
# Step 1: Upload files to all nodes
# ------------------------------------------------------------------------------
echo "[1/4] Uploading files to all nodes..."

for NODE_IP in "${NODE_IPS_ARRAY[@]}"; do
    echo "  Uploading to $NODE_IP..."
    ssh "$SSH_USER@$NODE_IP" "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR/bin $REMOTE_DIR/plugins"
    scp -q "$SCRIPT_DIR/bin/avalanchego" "$SSH_USER@$NODE_IP:$REMOTE_DIR/bin/"
    scp -q "$SCRIPT_DIR/bin/$SUBNET_EVM_ID" "$SSH_USER@$NODE_IP:$REMOTE_DIR/plugins/"
    scp -q "$SCRIPT_DIR/node-config.json" "$SSH_USER@$NODE_IP:$REMOTE_DIR/"
    scp -q "$SCRIPT_DIR/chain-config.json" "$SSH_USER@$NODE_IP:$REMOTE_DIR/"
done

echo "  Upload complete."

# ------------------------------------------------------------------------------
# Step 2: Start bootstrap node (first node - primary network)
# ------------------------------------------------------------------------------
echo "[2/4] Starting bootstrap primary node on $BOOTSTRAP_IP..."

ssh "$SSH_USER@$BOOTSTRAP_IP" bash -s "$BOOTSTRAP_IP" << 'BOOTSTRAP_EOF'
set -e
PUBLIC_IP=$1
cd ~/avalanche-benchmark

# Kill any existing avalanchego processes
pkill -f avalanchego || true
sleep 1

# Create data directories for all three node types
rm -rf data
mkdir -p data/primary/{db,logs}
mkdir -p data/validator/{db,logs}
mkdir -p data/rpc/{db,logs}

# Start PRIMARY NETWORK node (port 9650/9651)
nohup ./bin/avalanchego \
    --http-port=9650 \
    --staking-port=9651 \
    --http-host=0.0.0.0 \
    --public-ip=$PUBLIC_IP \
    --db-dir=data/primary/db \
    --log-dir=data/primary/logs \
    --data-dir=data/primary \
    --network-id=local \
    --sybil-protection-enabled=false \
    --plugin-dir="$(pwd)/plugins" \
    --config-file=node-config.json \
    --bootstrap-ips= \
    --bootstrap-ids= \
    >data/primary/logs/avalanchego.out 2>&1 &

disown
sleep 2
BOOTSTRAP_EOF

echo "  Waiting for bootstrap node ID..."

BOOTSTRAP_NODE_ID=""
for i in {1..60}; do
    RESULT=$(curl -s -X POST --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' -H 'Content-Type: application/json' "http://$BOOTSTRAP_IP:9650/ext/info" 2>/dev/null || true)
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
# Step 3: Start primary network nodes on remaining machines
# ------------------------------------------------------------------------------
if [ "$NODE_COUNT" -gt 1 ]; then
    echo "[3/4] Starting primary network nodes on other machines..."
else
    echo "[3/4] Single-node mode, no additional primary nodes to start."
fi

start_primary_node() {
    local NODE_IP=$1
    local NODE_NUM=$2

    echo "  Starting primary node $NODE_NUM on $NODE_IP..."

    ssh "$SSH_USER@$NODE_IP" bash -s "$BOOTSTRAP_NODE_ID" "$BOOTSTRAP_IP" "$NODE_IP" << 'PRIMARY_EOF'
set -e
BOOTSTRAP_NODE_ID=$1
BOOTSTRAP_IP=$2
PUBLIC_IP=$3

cd ~/avalanche-benchmark

# Kill any existing avalanchego processes
pkill -f avalanchego || true
sleep 1

# Create data directories for all three node types
rm -rf data
mkdir -p data/primary/{db,logs}
mkdir -p data/validator/{db,logs}
mkdir -p data/rpc/{db,logs}

# Start PRIMARY NETWORK node (port 9650/9651)
nohup ./bin/avalanchego \
    --http-port=9650 \
    --staking-port=9651 \
    --http-host=0.0.0.0 \
    --public-ip=$PUBLIC_IP \
    --db-dir=data/primary/db \
    --log-dir=data/primary/logs \
    --data-dir=data/primary \
    --network-id=local \
    --sybil-protection-enabled=false \
    --plugin-dir="$(pwd)/plugins" \
    --config-file=node-config.json \
    --bootstrap-ips=${BOOTSTRAP_IP}:9651 \
    --bootstrap-ids=${BOOTSTRAP_NODE_ID} \
    >data/primary/logs/avalanchego.out 2>&1 &

disown
sleep 2
PRIMARY_EOF
}

for i in $(seq 1 $((NODE_COUNT - 1))); do
    start_primary_node "${NODE_IPS_ARRAY[$i]}" $((i + 1))
done

# ------------------------------------------------------------------------------
# Step 4: Wait for all primary nodes to be healthy
# ------------------------------------------------------------------------------
echo "[4/4] Waiting for all primary nodes to be healthy..."

check_node_health() {
    local NODE_IP=$1
    local NODE_NAME=$2

    echo -n "  Waiting for $NODE_NAME ($NODE_IP:9650)..." >&2
    for i in {1..60}; do
        if curl -sf "http://$NODE_IP:9650/ext/health" >/dev/null 2>&1; then
            RESULT=$(curl -s -X POST --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' -H 'Content-Type: application/json' "http://$NODE_IP:9650/ext/info" || true)
            NODE_ID=$(echo "$RESULT" | grep -o '"nodeID":"[^"]*"' | cut -d'"' -f4 || true)
            echo " OK" >&2
            echo "$NODE_ID"
            return 0
        fi
        sleep 1
    done
    echo " TIMEOUT" >&2
    return 1
}

# Collect all node IDs
declare -a NODE_IDS
NODE_IDS[0]="$BOOTSTRAP_NODE_ID"

for i in $(seq 1 $((NODE_COUNT - 1))); do
    local_n=$((i + 1))
    NODE_ID=$(check_node_health "${NODE_IPS_ARRAY[$i]}" "node $local_n")
    if [ -z "$NODE_ID" ]; then
        echo "ERROR: Node $local_n failed to become healthy within 60s"
        echo "  Check logs: ssh ${NODE_IPS_ARRAY[$i]} 'tail -50 ~/avalanche-benchmark/data/primary/logs/main.log'"
        exit 1
    fi
    NODE_IDS[$i]="$NODE_ID"
done

echo ""
echo "=== Primary Network Bootstrap Complete ==="
echo ""
echo "Primary Network Nodes (port 9650):"
for i in "${!NODE_IPS_ARRAY[@]}"; do
    local_n=$((i + 1))
    label=""
    if [ "$i" -eq 0 ]; then label=" (bootstrap)"; fi
    echo "  Node $local_n: ${NODE_IPS_ARRAY[$i]} - ${NODE_IDS[$i]}$label"
done
echo ""
echo "Next step: ./02_create_l1.sh"
echo "  This will start validator (9652) and RPC (9654) nodes on each machine."
