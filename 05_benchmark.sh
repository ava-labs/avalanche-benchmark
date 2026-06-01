#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"

if [ ! -f "$NETWORK_ENV" ]; then
    echo "ERROR: network.env not found. Run 02_create_l1.sh first."
    exit 1
fi

source "$NETWORK_ENV"

if [ -z "$CHAIN_ID" ]; then
    echo "ERROR: CHAIN_ID not found in network.env"
    exit 1
fi

if [ "$#" -ne 0 ]; then
    echo "ERROR: 05_benchmark.sh does not accept flags. Edit the fixed settings in the script if needed."
    exit 2
fi

# Load generator: the prebuilt local bombard (bin/bombard, built by `make`).
# Using the prebuilt binary means the release ships and runs without the Go
# source on the operator box.
BOMBARD="$SCRIPT_DIR/bin/bombard"
if [ ! -x "$BOMBARD" ]; then
    echo "ERROR: $BOMBARD not found. Build it with 'make' (or: go build -o bin/bombard ./cmd/bombard)."
    exit 1
fi

# bombard broadcasts every tx to ALL pool RPCs (machines 1-4) over HTTP and
# ignores down nodes, so pass all four. A cordoned validator is just an endpoint
# whose sends are dropped — never a single point of failure.
RPC_URLS=()
for i in 0 1 2 3; do
    RPC_URLS+=("http://${NODE_IPS_ARRAY[$i]}:9652/ext/bc/$CHAIN_ID/rpc")
done
RPC_LIST="$(IFS=,; echo "${RPC_URLS[*]}")"

TARGET_RPS=1000
RESUBMIT_INTERVAL=3s

echo "=== Benchmark ==="
echo "Chain ID: $CHAIN_ID"
echo "Target:   $TARGET_RPS rps"
echo "Resubmit: $RESUBMIT_INTERVAL"
echo ""
echo "RPC endpoints (pool machines 1-4):"
for u in "${RPC_URLS[@]}"; do echo "  $u"; done
echo ""

exec "$BOMBARD" --rpc "$RPC_LIST" -rps "$TARGET_RPS" -resubmit "$RESUBMIT_INTERVAL"
