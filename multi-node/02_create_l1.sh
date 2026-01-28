#!/bin/bash
set -e

# Error handler to show what went wrong
trap 'echo "ERROR: Script failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"
NETWORK_ENV="$SCRIPT_DIR/network.env"
REMOTE_DIR="~/avalanche-benchmark"

# Port layout per machine:
#   Primary:   HTTP 9650, Staking 9651 (already running from 01_bootstrap)
#   Validator: HTTP 9652, Staking 9653
#   RPC:       HTTP 9654, Staking 9655

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
echo "[1/4] Creating L1..."
"$SCRIPT_DIR/bin/create-l1" -output "$NETWORK_ENV"

# Load the results
source "$NETWORK_ENV"

echo ""
echo "Subnet ID: $SUBNET_ID"
echo "Chain ID:  $CHAIN_ID"
echo ""

# ------------------------------------------------------------------------------
# Step 2: Get bootstrap node ID from primary network node
# ------------------------------------------------------------------------------
echo "[2/4] Getting bootstrap node ID..."

BOOTSTRAP_NODE_ID=$(curl -s -X POST --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' \
    -H 'Content-Type: application/json' "http://$NODE1_IP:9650/ext/info" | \
    grep -o '"nodeID":"[^"]*"' | cut -d'"' -f4)

if [ -z "$BOOTSTRAP_NODE_ID" ]; then
    echo "ERROR: Could not get bootstrap node ID from $NODE1_IP:9650"
    echo "Make sure 01_bootstrap_primary_network.sh has been run."
    exit 1
fi

echo "  Bootstrap node ID: $BOOTSTRAP_NODE_ID"

# ------------------------------------------------------------------------------
# Step 3: Start validator and RPC nodes on all machines
# ------------------------------------------------------------------------------
echo "[3/4] Starting validator and RPC nodes on all machines..."

start_l1_nodes() {
    local NODE_IP=$1
    local NODE_NUM=$2
    local BOOTSTRAP_ID=$3
    local BOOTSTRAP_IP=$4

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
    --plugin-dir=/home/ubuntu/avalanche-benchmark/plugins \\
    --config-file=node-config.json \\
    --chain-config-dir=data/validator/configs/chains \\
    --track-subnets="$SUBNET_ID" \\
    --bootstrap-ips=${BOOTSTRAP_IP}:9651 \\
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
    --plugin-dir=/home/ubuntu/avalanche-benchmark/plugins \\
    --config-file=node-config.json \\
    --chain-config-dir=data/rpc/configs/chains \\
    --track-subnets="$SUBNET_ID" \\
    --bootstrap-ips=${BOOTSTRAP_IP}:9651 \\
    --bootstrap-ids=${BOOTSTRAP_ID} \\
    >data/rpc/logs/avalanchego.out 2>&1 &
disown

sleep 2
echo "  Validator and RPC nodes started on $NODE_IP"
EOF

    # Copy and execute
    scp -q /tmp/start-l1-nodes-${NODE_NUM}.sh "$NODE_IP:~/avalanche-benchmark/start-l1-nodes.sh"
    ssh "$NODE_IP" "chmod +x ~/avalanche-benchmark/start-l1-nodes.sh && ~/avalanche-benchmark/start-l1-nodes.sh"
}

# Start on all three machines
start_l1_nodes "$NODE1_IP" 1 "$BOOTSTRAP_NODE_ID" "$NODE1_IP"
start_l1_nodes "$NODE2_IP" 2 "$BOOTSTRAP_NODE_ID" "$NODE1_IP"
start_l1_nodes "$NODE3_IP" 3 "$BOOTSTRAP_NODE_ID" "$NODE1_IP"

# ------------------------------------------------------------------------------
# Step 4: Verify RPC is working on all RPC nodes (port 9654)
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
verify_rpc "$NODE1_IP" 1 9652 "validator" || ((FAILED++))
verify_rpc "$NODE2_IP" 2 9652 "validator" || ((FAILED++))
verify_rpc "$NODE3_IP" 3 9652 "validator" || ((FAILED++))

# Verify RPC nodes (port 9654)
verify_rpc "$NODE1_IP" 1 9654 "rpc" || ((FAILED++))
verify_rpc "$NODE2_IP" 2 9654 "rpc" || ((FAILED++))
verify_rpc "$NODE3_IP" 3 9654 "rpc" || ((FAILED++))

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
echo "=== L1 Ready ==="
echo ""
echo "Subnet ID: $SUBNET_ID"
echo "Chain ID:  $CHAIN_ID"
echo ""
echo "Nodes per machine:"
echo "  Primary (9650)   - bootstraps the network"
echo "  Validator (9652) - validates L1 transactions"
echo "  RPC (9654)       - handles benchmark traffic"
echo ""
echo "RPC Endpoints (for benchmarking):"
echo "  http://$NODE1_IP:9654/ext/bc/$CHAIN_ID/rpc"
echo "  http://$NODE2_IP:9654/ext/bc/$CHAIN_ID/rpc"
echo "  http://$NODE3_IP:9654/ext/bc/$CHAIN_ID/rpc"
echo ""
echo "Next: Run ./03_monitoring.sh to deploy Prometheus + Grafana"
echo "      Then ./04_benchmark.sh to start benchmarking"
