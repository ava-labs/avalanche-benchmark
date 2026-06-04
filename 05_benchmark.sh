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

# Bombard ONLY the dedicated RPC node (machine 5, key 10 = zero-weight
# non-validator that tracks the subnet and serves RPC). It is PINNED: the
# failover engine never promotes it to a validator, so this clean ingress path
# survives failover events — unlike the hot spare m4 (key 9), which becomes a
# validator whenever one of m1-m3 goes down. Ingress on the consensus-critical
# validators (m1-m3) wedges/throttles consensus; routing all load through the
# dedicated non-validating RPC node keeps the validators healthy and holds
# ~4000 TPS glass-smooth (2026-06-04 submission-target comparison). See wiki:
# why_bombard_the_non_validating_rpc_tracker_not_the_validator_and_it_must_be_sybil_on.
TRACKER_IP="${NODE_IPS_ARRAY[4]}"
RPC_URLS=("http://${TRACKER_IP}:9652/ext/bc/$CHAIN_ID/rpc")
RPC_LIST="$(IFS=,; echo "${RPC_URLS[*]}")"

# Validated stable defaults (2026-06-03): bombard the m5 dedicated RPC node at
# 4000 rps with a SHALLOW inflight cap of 750. Inflight depth is the smoothness knob — deeper
# queues turn proposer-slot stalls into big post-stall bursts that provoke more
# stalls. inflight=750 sustains ~4.3k TPS glass-smooth (p50 ~130ms, 0% reject,
# no folds over 10 min). See devlog 2026-06-03 + wiki tracker note.
TARGET_RPS=4000
INFLIGHT=750
RESUBMIT_INTERVAL=5s
# Issue 1% over target. The rolling token-bucket pacer no longer leaks the tail
# of each second, but mined still reads a hair under target because ~240 txs are
# always in flight at any instant (mined = issued - inflight). A 1% overshoot
# absorbs that tail plus reject/jitter losses so mined lands at-or-above target;
# measured 4032 TPS, inflight ~244, 0 resubmits (2% starts grazing the cap).
OVERSHOOT=0.01

echo "=== Benchmark ==="
echo "Chain ID: $CHAIN_ID"
echo "Target:   $TARGET_RPS rps  (inflight cap $INFLIGHT, +$(echo "$OVERSHOOT*100" | bc)% overshoot)"
echo "Resubmit: $RESUBMIT_INTERVAL"
echo ""
echo "Ingress: dedicated RPC node (machine 5, key 10, pinned non-validator) only:"
for u in "${RPC_URLS[@]}"; do echo "  $u"; done
echo ""

exec "$BOMBARD" --rpc "$RPC_LIST" -rps "$TARGET_RPS" -inflight "$INFLIGHT" -resubmit "$RESUBMIT_INTERVAL" -overshoot "$OVERSHOOT"
