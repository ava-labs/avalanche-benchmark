#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

STAKING_DIR="$SCRIPT_DIR/staking"
NODE_IDS_FILE="$STAKING_DIR/node-ids.env"
DATA_ROOT="$SCRIPT_DIR/data/pchain"
PCHAIN_NODE_COUNT=5
PCHAIN_HTTP_BASE_PORT=9650
PCHAIN_STAKING_BASE_PORT=9651
PCHAIN_PORT_STEP=10

node_id_for_l1_index() {
    local idx=$1
    local value

    if [ ! -f "$NODE_IDS_FILE" ]; then
        echo "ERROR: missing $NODE_IDS_FILE" >&2
        exit 1
    fi

    value=$(grep -E "^L1_${idx}_NODE_ID=" "$NODE_IDS_FILE" | tail -n 1 | cut -d= -f2- || true)
    if [ -z "$value" ]; then
        echo "ERROR: missing L1_${idx}_NODE_ID in $NODE_IDS_FILE" >&2
        exit 1
    fi
    echo "$value"
}

pchain_http_port() {
    local idx=$1
    echo $((PCHAIN_HTTP_BASE_PORT + (idx - 1) * PCHAIN_PORT_STEP))
}

pchain_staking_port() {
    local idx=$1
    echo $((PCHAIN_STAKING_BASE_PORT + (idx - 1) * PCHAIN_PORT_STEP))
}

join_by_comma() {
    local IFS=,
    echo "$*"
}

pchain_bootstrap_ips_for_node() {
    local ips=()
    local i

    for i in $(seq 1 "$PCHAIN_NODE_COUNT"); do
        ips+=("127.0.0.1:$(pchain_staking_port "$i")")
    done
    join_by_comma "${ips[@]}"
}

pchain_bootstrap_ids_for_node() {
    local ids=()
    local i

    for i in $(seq 1 "$PCHAIN_NODE_COUNT"); do
        ids+=("$(node_id_for_l1_index "$i")")
    done
    join_by_comma "${ids[@]}"
}

require_file() {
    local path=$1
    if [ ! -f "$path" ]; then
        echo "ERROR: missing $path"
        exit 1
    fi
}

require_local_artifacts() {
    require_file "$SCRIPT_DIR/bin/avalanchego"
    require_file "$SCRIPT_DIR/node-config.json"

    local node_num
    for node_num in $(seq 1 "$PCHAIN_NODE_COUNT"); do
        require_file "$STAKING_DIR/l1/$node_num/staker.crt"
        require_file "$STAKING_DIR/l1/$node_num/staker.key"
        require_file "$STAKING_DIR/l1/$node_num/signer.key"
        node_id_for_l1_index "$node_num" >/dev/null
    done
}

discover_public_ip() {
    curl -fsS https://checkip.amazonaws.com | tr -d '[:space:]'
}

start_local_pchain_node() {
    local node_num=$1
    local http_port
    local staking_port
    local data_dir
    local args=()

    http_port=$(pchain_http_port "$node_num")
    staking_port=$(pchain_staking_port "$node_num")
    data_dir="$DATA_ROOT/$node_num"

    mkdir -p "$data_dir/db" "$data_dir/logs"

    args=(
        --http-port="$http_port"
        --staking-port="$staking_port"
        --http-host=127.0.0.1
        --public-ip="$PCHAIN_PUBLIC_IP"
        --db-dir="$data_dir/db"
        --log-dir="$data_dir/logs"
        --data-dir="$data_dir"
        --network-id=local
        --staking-tls-cert-file="$STAKING_DIR/l1/$node_num/staker.crt"
        --staking-tls-key-file="$STAKING_DIR/l1/$node_num/staker.key"
        --staking-signer-key-file="$STAKING_DIR/l1/$node_num/signer.key"
        --plugin-dir="$SCRIPT_DIR/plugins"
        --config-file="$SCRIPT_DIR/node-config.json"
        --bootstrap-ips="$(pchain_bootstrap_ips_for_node)"
        --bootstrap-ids="$(pchain_bootstrap_ids_for_node)"
    )

    echo "  pchain-$node_num: http=127.0.0.1:$http_port staking=$PCHAIN_PUBLIC_IP:$staking_port identity=staking/l1/$node_num"
    setsid "$SCRIPT_DIR/bin/avalanchego" "${args[@]}" >"$data_dir/logs/avalanchego.out" 2>&1 < /dev/null &
    disown
}

wait_for_local_pchain_node() {
    local node_num=$1
    local http_port
    local expected_node_id
    local result
    local node_id

    http_port=$(pchain_http_port "$node_num")
    expected_node_id=$(node_id_for_l1_index "$node_num")

    echo -n "  Waiting for pchain-$node_num (127.0.0.1:$http_port)..."
    for _ in {1..60}; do
        if curl -sf "http://127.0.0.1:$http_port/ext/health" >/dev/null 2>&1; then
            result=$(curl -s -X POST \
                --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' \
                -H 'Content-Type: application/json' \
                "http://127.0.0.1:$http_port/ext/info" || true)
            node_id=$(echo "$result" | grep -o '"nodeID":"[^"]*"' | cut -d'"' -f4 || true)
            if [ "$node_id" = "$expected_node_id" ]; then
                # Health + matching NodeID is not enough: the API answers before
                # the P-chain has finished bootstrapping. Require P bootstrapped
                # so we never declare "ready" on a node that can't yet serve the
                # chain the L1 validators bootstrap against.
                if curl -s -X POST \
                    --data '{"jsonrpc":"2.0","id":1,"method":"info.isBootstrapped","params":{"chain":"P"}}' \
                    -H 'Content-Type: application/json' \
                    "http://127.0.0.1:$http_port/ext/info" 2>/dev/null \
                    | grep -q '"isBootstrapped":true'; then
                    echo " OK"
                    return 0
                fi
            fi
        fi
        sleep 1
    done

    echo " TIMEOUT"
    echo "ERROR: pchain-$node_num did not become healthy + P-bootstrapped with expected NodeID $expected_node_id"
    echo "  Logs: $DATA_ROOT/$node_num/logs/main.log"
    return 1
}

# Verify the 5 local validators formed a full peer mesh. The L1 nodes can only
# clear AvalancheGo's >=75%-connected-stake startup latch if these P-chain
# validators are actually connected to each other; a node that is "healthy" but
# isolated (numPeers < N-1) is the classic cause of L1 nodes wedging in
# BOOTSTRAPPING downstream. Assert it here so the failure surfaces locally.
verify_pchain_mesh() {
    local expected=$((PCHAIN_NODE_COUNT - 1))
    local ok=0
    local node_num
    local http_port
    local num_peers

    echo "[verify] Checking P-chain peer mesh (expect numPeers=$expected per node)..."
    for node_num in $(seq 1 "$PCHAIN_NODE_COUNT"); do
        http_port=$(pchain_http_port "$node_num")
        num_peers=$(curl -s -X POST \
            --data '{"jsonrpc":"2.0","id":1,"method":"info.peers","params":{}}' \
            -H 'Content-Type: application/json' \
            "http://127.0.0.1:$http_port/ext/info" 2>/dev/null \
            | grep -o '"numPeers":"\?[0-9]*' | grep -o '[0-9]*' | head -n1)
        num_peers=${num_peers:-0}
        if [ "$num_peers" -ge "$expected" ]; then
            echo "  pchain-$node_num: numPeers=$num_peers OK"
        else
            echo "  pchain-$node_num: numPeers=$num_peers (expected >=$expected) NOT MESHED"
            ok=1
        fi
    done
    if [ "$ok" -ne 0 ]; then
        echo "ERROR: P-chain validators are not fully meshed. L1 nodes will likely"
        echo "       wedge in BOOTSTRAPPING (cannot reach >=75% connected stake)."
        return 1
    fi
    echo "  P-chain mesh OK."
    return 0
}

echo "=== Local Primary Network Bootstrap ==="
echo ""
echo "Starting five local-genesis P-chain validators on this machine."
echo "No remote SSH or artifact upload happens in this script."
echo ""

require_local_artifacts

PCHAIN_PUBLIC_IP="$(discover_public_ip)"
if [ -z "$PCHAIN_PUBLIC_IP" ]; then
    echo "ERROR: could not discover local public IP"
    exit 1
fi

echo "Local public IP: $PCHAIN_PUBLIC_IP"
echo ""

echo "[1/3] Cleaning local AvalancheGo processes and P-chain data..."
pkill -f avalanchego || true
sleep 1
rm -rf "$DATA_ROOT"
mkdir -p "$DATA_ROOT" "$SCRIPT_DIR/plugins"

echo "[2/3] Starting local P-chain validators..."
for node_num in $(seq 1 "$PCHAIN_NODE_COUNT"); do
    start_local_pchain_node "$node_num"
    sleep 1
done

echo "[3/3] Waiting for local P-chain validators..."
for node_num in $(seq 1 "$PCHAIN_NODE_COUNT"); do
    wait_for_local_pchain_node "$node_num"
done

echo ""
verify_pchain_mesh

echo ""
echo "=== Local Primary Network Ready ==="
for node_num in $(seq 1 "$PCHAIN_NODE_COUNT"); do
    echo "  pchain-$node_num: http://127.0.0.1:$(pchain_http_port "$node_num") staking=$PCHAIN_PUBLIC_IP:$(pchain_staking_port "$node_num") $(node_id_for_l1_index "$node_num")"
done
echo ""
echo "Next step: ./02_create_l1.sh"
