#!/bin/bash
set -e

trap 'echo "ERROR: Script failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"

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

echo "=== Start Remote L1 Nodes ==="
echo ""
echo "Subnet ID: $SUBNET_ID"
echo "Chain ID:  $CHAIN_ID"
echo "L1 validator identities: staking/l1/${L1_VALIDATOR_START_INDEX}..$((L1_VALIDATOR_START_INDEX + L1_VALIDATOR_COUNT - 1))"
echo "L1 Snow parameters: AvalancheGo defaults (k=20 alpha=15)"
echo "Terraform benchmark machines 1..$L1_VALIDATOR_COUNT run one L1 validator/RPC process each; P-chain runs locally."
echo ""

echo "[1/4] Checking local P-chain bootstrap node..."
BOOTSTRAP_NODE_ID=$(curl -s -X POST --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' \
    -H 'Content-Type: application/json' "http://127.0.0.1:9650/ext/info" | \
    grep -o '"nodeID":"[^"]*"' | cut -d'"' -f4)

EXPECTED_BOOTSTRAP_NODE_ID=$(node_id_for_l1_index 1)
if [ "$BOOTSTRAP_NODE_ID" != "$EXPECTED_BOOTSTRAP_NODE_ID" ]; then
    echo "ERROR: local pchain-1 NodeID mismatch: got ${BOOTSTRAP_NODE_ID:-empty} expected $EXPECTED_BOOTSTRAP_NODE_ID"
    echo "Run ./01_bootstrap_primary_network.sh first."
    exit 1
fi
echo "  pchain-1: $BOOTSTRAP_NODE_ID"

PCHAIN_PUBLIC_IP="$(pchain_public_ip)"
PCHAIN_BOOTSTRAP_IPS="$(pchain_public_staking_ips_csv "$PCHAIN_PUBLIC_IP")"
PCHAIN_BOOTSTRAP_IDS="$(pchain_node_ids_csv)"
echo "  P-chain staking bootstrap: $PCHAIN_BOOTSTRAP_IPS"

echo "[2/4] Cleaning benchmark nodes and uploading remote L1 artifacts..."
for i in "${!NODE_IPS_ARRAY[@]}"; do
    NODE_IP="${NODE_IPS_ARRAY[$i]}"
    VALIDATOR_KEY_INDEX=$((L1_VALIDATOR_START_INDEX + i))
    NODE_NUM=$((i + 1))

    echo "  Cleaning node $NODE_NUM ($NODE_IP)..."
    ssh "$SSH_USER@$NODE_IP" "pkill -x avalanchego || true; rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR/bin $REMOTE_DIR/plugins $REMOTE_DIR/staking/l1"

    if [ "$NODE_NUM" -gt "$L1_VALIDATOR_COUNT" ]; then
        continue
    fi

    echo "  Uploading validator artifacts to node $NODE_NUM ($NODE_IP)..."
    scp -q "$SCRIPT_DIR/bin/avalanchego" "$SSH_USER@$NODE_IP:$REMOTE_DIR/bin/"
    scp -q "$SCRIPT_DIR/bin/$SUBNET_EVM_ID" "$SSH_USER@$NODE_IP:$REMOTE_DIR/plugins/"
    scp -q "$SCRIPT_DIR/node-config.json" "$SSH_USER@$NODE_IP:$REMOTE_DIR/"
    scp -q "$SCRIPT_DIR/chain-config.json" "$SSH_USER@$NODE_IP:$REMOTE_DIR/"
    scp -q -r "$SCRIPT_DIR/staking/l1/$VALIDATOR_KEY_INDEX" "$SSH_USER@$NODE_IP:$REMOTE_DIR/staking/l1/"
done
echo "  Upload complete."

l1_bootstrap_ips_for_node() {
    local NODE_NUM=$1
    local ips=()
    local i

    IFS=',' read -ra ips <<< "$PCHAIN_BOOTSTRAP_IPS"
    for i in $(seq 0 $((L1_VALIDATOR_COUNT - 1))); do
        if [ $((i + 1)) -eq "$NODE_NUM" ]; then
            continue
        fi
        ips+=("${NODE_IPS_ARRAY[$i]}:9653")
    done
    join_by_comma "${ips[@]}"
}

l1_bootstrap_ids_for_node() {
    local NODE_NUM=$1
    local ids=()
    local i
    local key_idx

    IFS=',' read -ra ids <<< "$PCHAIN_BOOTSTRAP_IDS"
    for i in $(seq 1 "$L1_VALIDATOR_COUNT"); do
        if [ "$i" -eq "$NODE_NUM" ]; then
            continue
        fi
        key_idx=$((L1_VALIDATOR_START_INDEX + i - 1))
        ids+=("$(node_id_for_l1_index "$key_idx")")
    done
    join_by_comma "${ids[@]}"
}

start_l1_validator() {
    local NODE_IP=$1
    local NODE_NUM=$2
    local VALIDATOR_KEY_INDEX=$((L1_VALIDATOR_START_INDEX + NODE_NUM - 1))
    local L1_BOOTSTRAP_IPS
    local L1_BOOTSTRAP_IDS

    L1_BOOTSTRAP_IPS=$(l1_bootstrap_ips_for_node "$NODE_NUM")
    L1_BOOTSTRAP_IDS=$(l1_bootstrap_ids_for_node "$NODE_NUM")

    echo "  Starting L1 validator $NODE_NUM on $NODE_IP (staking/l1/$VALIDATOR_KEY_INDEX)..."

    cat > /tmp/start-l1-validator-${NODE_NUM}.sh << EOF
#!/bin/bash
set -e
cd ~/avalanche-benchmark

rm -rf data/validator
mkdir -p "data/validator/configs/chains/$CHAIN_ID" "data/validator/db" "data/validator/logs"
cp chain-config.json "data/validator/configs/chains/$CHAIN_ID/config.json"

setsid ./bin/avalanchego \\
    --http-port=9652 \\
    --staking-port=9653 \\
    --http-host=0.0.0.0 \\
    --public-ip=$NODE_IP \\
    --db-dir=data/validator/db \\
    --log-dir=data/validator/logs \\
    --data-dir=data/validator \\
    --network-id=local \\
    --staking-tls-cert-file=staking/l1/$VALIDATOR_KEY_INDEX/staker.crt \\
    --staking-tls-key-file=staking/l1/$VALIDATOR_KEY_INDEX/staker.key \\
    --staking-signer-key-file=staking/l1/$VALIDATOR_KEY_INDEX/signer.key \\
    --plugin-dir=\$(pwd)/plugins \\
    --config-file=node-config.json \\
    --chain-config-dir=data/validator/configs/chains \\
    --track-subnets="$SUBNET_ID" \\
    --bootstrap-ips=$L1_BOOTSTRAP_IPS \\
    --bootstrap-ids=$L1_BOOTSTRAP_IDS \\
    >data/validator/logs/avalanchego.out 2>&1 < /dev/null &
EOF

    scp -q /tmp/start-l1-validator-${NODE_NUM}.sh "$SSH_USER@$NODE_IP:~/avalanche-benchmark/start-l1-validator.sh"
    ssh "$SSH_USER@$NODE_IP" "chmod +x ~/avalanche-benchmark/start-l1-validator.sh && ~/avalanche-benchmark/start-l1-validator.sh"
}

echo "[3/4] Starting remote L1 validators..."
for i in $(seq 0 $((L1_VALIDATOR_COUNT - 1))); do
    start_l1_validator "${NODE_IPS_ARRAY[$i]}" $((i + 1))
done

echo "[4/4] Verifying L1 RPC endpoints..."
verify_rpc() {
    local NODE_IP=$1
    local NODE_NUM=$2
    local PORT=$3
    local LABEL=$4
    local RPC_URL="http://$NODE_IP:$PORT/ext/bc/$CHAIN_ID/rpc"
    local HTTP_CODE
    local RESULT
    local HEX_CHAIN_ID
    local DEC_CHAIN_ID

    echo "  L1 $LABEL $NODE_NUM ($NODE_IP:$PORT):"

    for i in {1..24}; do
        HTTP_CODE=$(curl -s -m 3 -o /tmp/rpc_response_$$ -w "%{http_code}" -X POST \
            --data '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}' \
            -H 'Content-Type: application/json' "$RPC_URL" 2>/dev/null || echo "000")
        RESULT=$(cat /tmp/rpc_response_$$ 2>/dev/null || true)

        if echo "$RESULT" | grep -q '"result":"0x'; then
            HEX_CHAIN_ID=$(echo "$RESULT" | grep -o '"result":"0x[^"]*"' | cut -d'"' -f4)
            DEC_CHAIN_ID=$((HEX_CHAIN_ID))
            echo "    OK (chain ID: $HEX_CHAIN_ID / $DEC_CHAIN_ID)"
            rm -f /tmp/rpc_response_$$
            return 0
        fi

        HTTP_CODE="${HTTP_CODE: -3}"
        if [ "$HTTP_CODE" = "000" ]; then
            echo "    [$i/24] connection refused"
        elif [ "$HTTP_CODE" = "404" ]; then
            echo "    [$i/24] 404 - chain not ready"
        elif echo "$RESULT" | grep -q '"error"'; then
            ERR_MSG=$(echo "$RESULT" | grep -o '"message":"[^"]*"' | cut -d'"' -f4 | head -c 80)
            echo "    [$i/24] error: $ERR_MSG"
        else
            echo "    [$i/24] http $HTTP_CODE - waiting..."
        fi

        sleep 5
    done

    echo "    FAILED after 120s"
    echo "    Last response: $RESULT"
    rm -f /tmp/rpc_response_$$
    return 1
}

FAILED=0
for i in $(seq 0 $((L1_VALIDATOR_COUNT - 1))); do
    verify_rpc "${NODE_IPS_ARRAY[$i]}" $((i + 1)) 9652 validator || FAILED=$((FAILED + 1))
done

if [ "$FAILED" -gt 0 ]; then
    echo ""
    echo "ERROR: $FAILED L1 RPC endpoint(s) failed verification"
    echo "  Validator logs: ssh <NODE_IP> 'tail -100 ~/avalanche-benchmark/data/validator/logs/main.log'"
    exit 1
fi

echo ""
echo "=== Remote L1 Nodes Started ==="
echo ""
echo "Validator RPC endpoints:"
for i in $(seq 0 $((L1_VALIDATOR_COUNT - 1))); do
    NODE_IP="${NODE_IPS_ARRAY[$i]}"
    echo "  http://$NODE_IP:9652/ext/bc/$CHAIN_ID/rpc"
done
echo ""
echo "Next: Run ./05_benchmark.sh"
