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

# Bombard BOTH dedicated archive RPC nodes (m5+m6, keys 10+19 = zero-weight
# non-validators that track the subnet and serve RPC). They are PINNED: the
# failover engine never promotes them to validators, so this clean ingress path
# survives failover events — unlike the hot spare m4 (key 9), which becomes a
# validator whenever one of m1-m3 goes down. Ingress on the consensus-critical
# validators (m1-m3) wedges/throttles consensus; routing all load through the
# dedicated non-validating RPC nodes keeps the validators healthy and holds
# ~4000 TPS glass-smooth (2026-06-04 submission-target comparison). See wiki:
# why_bombard_the_non_validating_rpc_tracker_not_the_validator_and_it_must_be_sybil_on.
RPC_URLS=(
    "http://${NODE_IPS_ARRAY[4]}:9652/ext/bc/$CHAIN_ID/rpc"
    "http://${NODE_IPS_ARRAY[5]}:9652/ext/bc/$CHAIN_ID/rpc"
)

# Two-site mode: also feed bombard the backup site's pinned archive RPCs (b5+b6).
# Bombard is failover-native — it fans sends across reachable endpoints, runs a
# watcher per endpoint, and resubmits in-flight txs — so with all four pinned RPCs
# listed the benchmark rides through a full site failover and the latency
# report captures the recovery window. See docs/two-site-failover.md.
if [ -n "$BACKUP_SITE_NODE_IPS" ]; then
    RPC_URLS+=(
        "http://${BACKUP_SITE_IPS_ARRAY[4]}:9652/ext/bc/$CHAIN_ID/rpc"
        "http://${BACKUP_SITE_IPS_ARRAY[5]}:9652/ext/bc/$CHAIN_ID/rpc"
    )
fi
RPC_LIST="$(IFS=,; echo "${RPC_URLS[*]}")"

# Full throughput. Throttling rps was a dead end — block rate is set by the block
# cadence (min-delay-target / initialMinDelayMS), not by rps, so the cross-region
# standby could never keep up at any useful rps. The fix is the block cadence: at
# ~30ms blocks the active site produces ~22 blk/s (under site B's ~43 blk/s
# consensus-follow ceiling), so B stays synced (keep-up ratio -> 1.0) at FULL rps.
# Throughput is gas-bound, not block-rate-bound, so fat 30ms blocks still sustain 4000+
# (measured: blocks ~1% full at 4000 rps — the chain is nowhere near the gas limit).
#
# INFLIGHT must scale with per-tx latency, not stay at the fast-block value. By Little's
# law mined_tps = inflight / latency; the 30ms cadence raised submit->mined latency to
# ~330ms, so the old 750 cap throttled to ~2300 tps (chain starved, mempool ~empty).
# By mined_tps = inflight/latency, the cap must cover the WORST-latency window, not just
# the fast same-site path. On site A (25ms, same-region) latency ~0.33s, so 2000 already
# clears 4000. But during failover the validators run on the cross-region backup at 100ms,
# pushing submit->mined latency to ~0.8s — there 2000/0.8 ≈ 2400 and the cap throttles
# mined below target. 8000 keeps the rps limiter binding (8000/0.8 ≈ 10000 headroom) so
# mined holds ~4000 through the failover too. Blocks are ~1% full at 4000 rps, so the chain
# absorbs the bigger inflight fine. Resubmit set above the worst failover proposer stall.
TARGET_RPS=4000
INFLIGHT=8000
RESUBMIT_INTERVAL=8s
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
echo "Ingress: pinned dedicated RPC node(s) — never promoted to validators:"
for u in "${RPC_URLS[@]}"; do echo "  $u"; done
echo ""

exec "$BOMBARD" --rpc "$RPC_LIST" -rps "$TARGET_RPS" -inflight "$INFLIGHT" -resubmit "$RESUBMIT_INTERVAL" -overshoot "$OVERSHOOT"
