#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$SCRIPT_DIR/_common.sh"

if [ ! -f "$NETWORK_ENV" ]; then
    echo "ERROR: network.env not found. Run ./setup/02_create_chain.sh first."
    exit 1
fi

source "$NETWORK_ENV"

if [ -z "$CHAIN_ID" ]; then
    echo "ERROR: CHAIN_ID not found in network.env"
    exit 1
fi

if [ "$#" -ne 0 ]; then
    echo "ERROR: 03_bombard.sh does not accept flags. Edit the fixed settings in the script if needed."
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

# Bombard the PINNED dedicated archive RPC nodes (role=rpc: m5+m6 on site A, plus
# b5+b6 in two-site mode - zero-weight non-validators that track
# the subnet and serve RPC). The failover engine never promotes them to validators,
# so this clean ingress path survives failover events - unlike the hot spare m4
# (key 9), which becomes a validator whenever one of m1-m3 goes down. Ingress on the
# consensus-critical validators (m1-m3) wedges/throttles consensus; routing all load
# through the dedicated non-validating RPC nodes keeps the validators healthy and
# holds ~4000 TPS glass-smooth (2026-06-04 submission-target comparison). Listing all
# four pinned RPCs lets bombard (failover-native: fans sends, watches each endpoint,
# resubmits in-flight txs) ride through a full site failover. See wiki:
# why_bombard_the_non_validating_rpc_tracker_not_the_validator_and_it_must_be_sybil_on.
#
# Endpoints come from `reconcile endpoints` (the single source of truth for the
# co-location-aware ports), so a co-located RPC slot is targeted on its ACTUAL port
# rather than a hardcoded :9652 (which would hit a validator on a shared box).
RECONCILE_BIN="$SCRIPT_DIR/bin/benchmark-fleet"
export NODE_IPS BACKUP_SITE_NODE_IPS
mapfile -t RPC_URLS < <("$RECONCILE_BIN" endpoints | awk -F'\t' -v c="$CHAIN_ID" \
    '$3=="rpc"{printf "http://%s:%s/ext/bc/%s/rpc\n", $4, $5, c}')
if [ "${#RPC_URLS[@]}" -eq 0 ]; then
    echo "ERROR: 'reconcile endpoints' returned no RPC nodes - check NODE_IPS/bin/benchmark-fleet." >&2
    exit 1
fi
RPC_LIST="$(IFS=,; echo "${RPC_URLS[*]}")"

# Full throughput. Throttling rps was a dead end - block rate is set by the block
# cadence (min-delay-target / initialMinDelayMS), not by rps, so the cross-region
# standby could never keep up at any useful rps. The fix is the block cadence: at
# ~30ms blocks the active site produces ~22 blk/s (under site B's ~43 blk/s
# consensus-follow ceiling), so B stays synced (keep-up ratio -> 1.0) at FULL rps.
# Throughput is gas-bound, not block-rate-bound, so fat 30ms blocks still sustain 4000+
# (measured: blocks ~1% full at 4000 rps - the chain is nowhere near the gas limit).
#
# INFLIGHT must scale with per-tx latency, not stay at the fast-block value. By Little's
# law mined_tps = inflight / latency; the 30ms cadence raised submit->mined latency to
# ~330ms, so the old 750 cap throttled to ~2300 tps (chain starved, mempool ~empty).
# 2000 gives mined headroom (~6000) so the rps limiter - not the inflight cap - binds.
TARGET_RPS=4000
INFLIGHT=2000
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
echo "Ingress: pinned dedicated RPC node(s) - never promoted to validators:"
for u in "${RPC_URLS[@]}"; do echo "  $u"; done
echo ""

exec "$BOMBARD" --rpc "$RPC_LIST" -rps "$TARGET_RPS" -inflight "$INFLIGHT" -resubmit "$RESUBMIT_INTERVAL" -overshoot "$OVERSHOOT" -key "$FUJI_WALLET_KEY"
