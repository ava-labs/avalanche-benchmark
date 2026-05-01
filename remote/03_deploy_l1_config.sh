#!/bin/bash
set -e

# Error handler to show what went wrong
trap 'echo "ERROR: Script failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"

# Port layout per machine:
#   Primary:     HTTP 9650, Staking 9651 (already running from 01_bootstrap)
#   Validator:   HTTP 9652, Staking 9653
#   RPC:         HTTP 9654, Staking 9655 (bombard traffic, locked-down hosts)
#   Archive-RPC: HTTP 9656, Staking 9657 (bootstrap only, no pruning,
#                                        --http-allowed-hosts=*, used by Blockscout)

# ------------------------------------------------------------------------------
# Load L1 network info
# ------------------------------------------------------------------------------
if [ ! -f "$NETWORK_ENV" ]; then
    echo "ERROR: network.env not found"
    echo ""
    echo "Run ./02_create_l1.sh first to create the L1."
    exit 1
fi

source "$NETWORK_ENV"

if [ -z "$SUBNET_ID" ] || [ -z "$CHAIN_ID" ]; then
    echo "ERROR: SUBNET_ID or CHAIN_ID not set in network.env"
    exit 1
fi

echo "=== Deploy L1 Config ==="
echo ""
echo "Subnet ID: $SUBNET_ID"
echo "Chain ID:  $CHAIN_ID"
echo ""
echo "This will restart validator and RPC nodes with the current chain-config.json"
echo ""

# ------------------------------------------------------------------------------
# Step 1: Get bootstrap node ID from primary network node
# ------------------------------------------------------------------------------
echo "[1/4] Getting bootstrap node ID..."

BOOTSTRAP_NODE_ID=$(curl -s -X POST --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' \
    -H 'Content-Type: application/json' "http://$BOOTSTRAP_IP:9650/ext/info" | \
    grep -o '"nodeID":"[^"]*"' | cut -d'"' -f4)

if [ -z "$BOOTSTRAP_NODE_ID" ]; then
    echo "ERROR: Could not get bootstrap node ID from $BOOTSTRAP_IP:9650"
    echo "Make sure 01_bootstrap_primary_network.sh has been run."
    exit 1
fi

echo "  Bootstrap node ID: $BOOTSTRAP_NODE_ID"

# ------------------------------------------------------------------------------
# Step 2: Upload chain config to all nodes
# ------------------------------------------------------------------------------
echo "[2/4] Uploading chain-config.json to all nodes..."

for NODE_IP in "${NODE_IPS_ARRAY[@]}"; do
    echo "  Uploading to $NODE_IP..."
    scp -q "$SCRIPT_DIR/chain-config.json" "$SSH_USER@$NODE_IP:$REMOTE_DIR/"
done

echo "  Upload complete."

# ------------------------------------------------------------------------------
# Step 3: Start validator and RPC nodes on all machines
# ------------------------------------------------------------------------------
echo "[3/4] Starting validator and RPC nodes on all machines..."

start_l1_nodes() {
    local NODE_IP=$1
    local NODE_NUM=$2
    local BOOTSTRAP_ID=$3
    local BOOTSTRAP_NODE_IP=$4

    echo "  Starting validator + RPC on $NODE_IP..."

    # Write the startup script locally then copy it
    cat > /tmp/start-l1-nodes-${NODE_NUM}.sh << EOF
#!/bin/bash
set -e
cd ~/avalanche-benchmark

# Generated: $(date)
# Node: $NODE_NUM ($NODE_IP)
# SUBNET_ID: $SUBNET_ID
# CHAIN_ID: $CHAIN_ID

# Kill existing validator and RPC nodes (but NOT primary)
pkill -f "data-dir=data/validator" || true
pkill -f "data-dir=data/rpc" || true
sleep 2

# Install chain config for validator
mkdir -p "data/validator/configs/chains/$CHAIN_ID"
cp chain-config.json "data/validator/configs/chains/$CHAIN_ID/config.json"

# Install chain config for RPC
mkdir -p "data/rpc/configs/chains/$CHAIN_ID"
cp chain-config.json "data/rpc/configs/chains/$CHAIN_ID/config.json"

# Start VALIDATOR node (port 9652/9653)
nohup ./bin/avalanchego \\
    --http-port=9652 \\
    --staking-port=9653 \\
    --http-host=0.0.0.0 \\
    --public-ip=$NODE_IP \\
    --db-dir=data/validator/db \\
    --log-dir=data/validator/logs \\
    --data-dir=data/validator \\
    --network-id=local \\
    --sybil-protection-enabled=false \\
    --plugin-dir=\$(pwd)/plugins \\
    --config-file=node-config.json \\
    --chain-config-dir=data/validator/configs/chains \\
    --track-subnets="$SUBNET_ID" \\
    --bootstrap-ips=${BOOTSTRAP_NODE_IP}:9651 \\
    --bootstrap-ids=${BOOTSTRAP_ID} \\
    >data/validator/logs/avalanchego.out 2>&1 &
disown

# Start RPC node (port 9654/9655)
nohup ./bin/avalanchego \\
    --http-port=9654 \\
    --staking-port=9655 \\
    --http-host=0.0.0.0 \\
    --public-ip=$NODE_IP \\
    --db-dir=data/rpc/db \\
    --log-dir=data/rpc/logs \\
    --data-dir=data/rpc \\
    --network-id=local \\
    --sybil-protection-enabled=false \\
    --plugin-dir=\$(pwd)/plugins \\
    --config-file=node-config.json \\
    --chain-config-dir=data/rpc/configs/chains \\
    --track-subnets="$SUBNET_ID" \\
    --bootstrap-ips=${BOOTSTRAP_NODE_IP}:9651 \\
    --bootstrap-ids=${BOOTSTRAP_ID} \\
    >data/rpc/logs/avalanchego.out 2>&1 &
disown

sleep 2
echo "  Validator and RPC nodes started on $NODE_IP"
EOF

    # Copy and execute
    scp -q /tmp/start-l1-nodes-${NODE_NUM}.sh "$SSH_USER@$NODE_IP:~/avalanche-benchmark/start-l1-nodes.sh"
    ssh "$SSH_USER@$NODE_IP" "chmod +x ~/avalanche-benchmark/start-l1-nodes.sh && ~/avalanche-benchmark/start-l1-nodes.sh"
}

# Start on all machines
for i in "${!NODE_IPS_ARRAY[@]}"; do
    start_l1_nodes "${NODE_IPS_ARRAY[$i]}" $((i + 1)) "$BOOTSTRAP_NODE_ID" "$BOOTSTRAP_IP"
done

# ------------------------------------------------------------------------------
# Start dedicated Archive-RPC node on the bootstrap host. Bound to 0.0.0.0
# with --http-allowed-hosts=* and an archive-mode chain config (pruning-enabled
# false, debug-tracer API exposed) so a Blockscout container on the operator's
# machine can index it. The bombard RPC nodes above stay locked down.
# ------------------------------------------------------------------------------
start_l1_archive_rpc_node() {
    local NODE_IP=$1
    local BOOTSTRAP_ID=$2
    local BOOTSTRAP_NODE_IP=$3

    echo "  Starting Archive-RPC node on $NODE_IP (bootstrap host)..."

    cat > /tmp/start-l1-archive-rpc.sh << EOF
#!/bin/bash
set -e
cd ~/avalanche-benchmark

# Generated: $(date)
# SUBNET_ID: $SUBNET_ID
# CHAIN_ID:  $CHAIN_ID

pkill -f "data-dir=data/archive-rpc" || true
sleep 2

mkdir -p "data/archive-rpc/configs/chains/$CHAIN_ID"

# Write an archive-mode chain config (no pruning, debug-tracer enabled).
# This applies only to the Archive-RPC node — bombard's RPCs keep
# the lighter shared chain-config.json.
python3 - <<'PY' > "data/archive-rpc/configs/chains/$CHAIN_ID/config.json"
import json, sys
with open("chain-config.json") as f:
    cfg = json.load(f)
cfg["pruning-enabled"] = False
cfg["eth-apis"] = [
    "eth", "eth-filter", "net", "web3",
    "internal-eth", "internal-blockchain", "internal-transaction",
    "debug-tracer",
]
print(json.dumps(cfg, indent=2))
PY

nohup ./bin/avalanchego \\
    --http-port=9656 \\
    --staking-port=9657 \\
    --http-host=0.0.0.0 \\
    --http-allowed-hosts="*" \\
    --public-ip=$NODE_IP \\
    --db-dir=data/archive-rpc/db \\
    --log-dir=data/archive-rpc/logs \\
    --data-dir=data/archive-rpc \\
    --network-id=local \\
    --sybil-protection-enabled=false \\
    --plugin-dir=\$(pwd)/plugins \\
    --config-file=node-config.json \\
    --chain-config-dir=data/archive-rpc/configs/chains \\
    --track-subnets="$SUBNET_ID" \\
    --bootstrap-ips=${BOOTSTRAP_NODE_IP}:9651 \\
    --bootstrap-ids=${BOOTSTRAP_ID} \\
    >data/archive-rpc/logs/avalanchego.out 2>&1 &
disown

sleep 2
echo "  Archive-RPC node started on $NODE_IP:9656"
EOF

    scp -q /tmp/start-l1-archive-rpc.sh "$SSH_USER@$NODE_IP:~/avalanche-benchmark/start-l1-archive-rpc.sh"
    ssh "$SSH_USER@$NODE_IP" "chmod +x ~/avalanche-benchmark/start-l1-archive-rpc.sh && ~/avalanche-benchmark/start-l1-archive-rpc.sh"
}

start_l1_archive_rpc_node "$BOOTSTRAP_IP" "$BOOTSTRAP_NODE_ID" "$BOOTSTRAP_IP"

# Persist the Archive-RPC URL for blockscout.sh to pick up.
ARCHIVE_RPC_URL="http://${BOOTSTRAP_IP}:9656/ext/bc/${CHAIN_ID}/rpc"
if grep -q '^ARCHIVE_RPC_URL=' "$NETWORK_ENV"; then
    sed -i.bak "s|^ARCHIVE_RPC_URL=.*|ARCHIVE_RPC_URL=${ARCHIVE_RPC_URL}|" "$NETWORK_ENV" && rm -f "$NETWORK_ENV.bak"
else
    echo "ARCHIVE_RPC_URL=${ARCHIVE_RPC_URL}" >> "$NETWORK_ENV"
fi
echo "  Saved ARCHIVE_RPC_URL to network.env"

# ------------------------------------------------------------------------------
# Step 4: Verify RPC is working on all nodes
# ------------------------------------------------------------------------------
echo ""
echo "[4/4] Verifying RPC endpoints on all nodes..."

verify_rpc() {
    local NODE_IP=$1
    local NODE_NUM=$2
    local PORT=$3
    local ROLE=$4
    local RPC_URL="http://$NODE_IP:$PORT/ext/bc/$CHAIN_ID/rpc"

    echo "  Node $NODE_NUM $ROLE ($NODE_IP:$PORT):"

    for i in {1..18}; do
        HTTP_CODE=$(curl -s -m 3 -o /tmp/rpc_response_$$ -w "%{http_code}" -X POST \
            --data '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}' \
            -H 'Content-Type: application/json' "$RPC_URL" 2>/dev/null || echo "000")
        RESULT=$(cat /tmp/rpc_response_$$ 2>/dev/null || true)

        # Success case
        if echo "$RESULT" | grep -q '"result":"0x'; then
            HEX_CHAIN_ID=$(echo "$RESULT" | grep -o '"result":"0x[^"]*"' | cut -d'"' -f4)
            DEC_CHAIN_ID=$((HEX_CHAIN_ID))
            echo "    OK (chain ID: $HEX_CHAIN_ID / $DEC_CHAIN_ID)"
            rm -f /tmp/rpc_response_$$
            return 0
        fi

        # Show status (take last 3 chars of HTTP_CODE in case of duplicates)
        HTTP_CODE="${HTTP_CODE: -3}"
        if [ "$HTTP_CODE" = "000" ]; then
            echo "    [$i/18] connection refused"
        elif [ "$HTTP_CODE" = "404" ]; then
            echo "    [$i/18] 404 - chain not ready"
        elif echo "$RESULT" | grep -q '"error"'; then
            ERR_MSG=$(echo "$RESULT" | grep -o '"message":"[^"]*"' | cut -d'"' -f4 | head -c 40)
            echo "    [$i/18] error: $ERR_MSG"
        else
            echo "    [$i/18] http $HTTP_CODE - waiting..."
        fi

        sleep 5
    done

    echo "    FAILED after 90s"
    echo "    Last response: $RESULT"
    rm -f /tmp/rpc_response_$$
    return 1
}

FAILED=0

# Verify validators (port 9652)
for i in "${!NODE_IPS_ARRAY[@]}"; do
    verify_rpc "${NODE_IPS_ARRAY[$i]}" $((i + 1)) 9652 "validator" || FAILED=$((FAILED + 1))
done

# Verify RPC nodes (port 9654)
for i in "${!NODE_IPS_ARRAY[@]}"; do
    verify_rpc "${NODE_IPS_ARRAY[$i]}" $((i + 1)) 9654 "rpc" || FAILED=$((FAILED + 1))
done

# Verify Archive-RPC node on bootstrap (port 9656)
verify_rpc "$BOOTSTRAP_IP" 1 9656 "archive-rpc" || FAILED=$((FAILED + 1))

if [ "$FAILED" -gt 0 ]; then
    echo ""
    echo "ERROR: $FAILED node(s) failed RPC verification"
    echo ""
    echo "Troubleshooting:"
    echo "  Validator logs: ssh <NODE_IP> 'tail -100 ~/avalanche-benchmark/data/validator/logs/main.log'"
    echo "  RPC logs:       ssh <NODE_IP> 'tail -100 ~/avalanche-benchmark/data/rpc/logs/main.log'"
    exit 1
fi

echo ""
echo "=== L1 Config Deployed ==="
echo ""
echo "Subnet ID: $SUBNET_ID"
echo "Chain ID:  $CHAIN_ID"
echo ""
echo "Nodes per machine:"
echo "  Primary (9650)   - bootstraps the network"
echo "  Validator (9652) - validates L1 transactions"
echo "  RPC (9654)       - handles benchmark traffic (locked-down host header)"
echo ""
echo "RPC Endpoints (for benchmarking):"
for NODE_IP in "${NODE_IPS_ARRAY[@]}"; do
    echo "  http://$NODE_IP:9654/ext/bc/$CHAIN_ID/rpc"
done
echo ""
echo "Archive-RPC Endpoint (for Blockscout, bootstrap host only):"
echo "  $ARCHIVE_RPC_URL"
echo ""
echo "Next: Run ./04_monitoring.sh to deploy Prometheus + Grafana"
echo "      Then ./05_benchmark.sh to start benchmarking"
