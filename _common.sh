#!/bin/bash
# Common configuration loader for remote benchmark scripts.
# Source this from other scripts after setting SCRIPT_DIR:
#
#   SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#   source "$SCRIPT_DIR/_common.sh"

ENV_FILE="$SCRIPT_DIR/.env"
NETWORK_ENV="$SCRIPT_DIR/network.env"
REMOTE_DIR="~/avalanche-benchmark"
SSH_KEY_PATH_DEFAULT="/home/ubuntu/.ssh/ilya-solohin-failover-bench-2026-05-04"

# Load .env
if [ ! -f "$ENV_FILE" ]; then
    echo "ERROR: .env file not found"
    echo ""
    echo "Create .env with your node IPs:"
    echo "  cp .env.example .env"
    echo "  # Edit .env and set NODE_IPS"
    exit 1
fi

source "$ENV_FILE"

# Export REMOTE_DIR so the reconcile child inherits the SAME deploy dir the shell
# scripts use. Lets .env point the deploy at a dir OTHER than the repo — required
# for a localhost run (REMOTE_DIR=repo would scp binaries onto themselves).
export REMOTE_DIR

SSH_KEY_PATH="${SSH_KEY_PATH:-$SSH_KEY_PATH_DEFAULT}"
SSH_OPTS=(
    -i "$SSH_KEY_PATH"
    -o IdentitiesOnly=yes
    -o IdentityAgent=none
    -o UserKnownHostsFile=/dev/null
    -o StrictHostKeyChecking=no
    -o ControlMaster=no
    -o ControlPath=none
    -o LogLevel=ERROR
    # Fail fast on a wedged host instead of hanging forever: bound the
    # connect/banner phase, and bail (~60s) if an established connection goes
    # dead mid-command. Kept in sync with cmd/reconcile/remote.go sshArgs.
    -o ConnectTimeout=10
    -o ServerAliveInterval=15
    -o ServerAliveCountMax=4
)

ssh() {
    command ssh "${SSH_OPTS[@]}" "$@"
}

scp() {
    command scp "${SSH_OPTS[@]}" "$@"
}

# Validate SSH_USER
if [ -z "$SSH_USER" ]; then
    echo "ERROR: SSH_USER not set in .env"
    exit 1
fi

# Topology config. PREFERRED: explicit per-role IP lists per data center —
#   VALIDATOR_IPS / SPARE_IPS / RPC_IPS           (site A / DC A)
#   BACKUP_VALIDATOR_IPS / BACKUP_SPARE_IPS / BACKUP_RPC_IPS  (site B / DC B, optional)
# Both data centers run LIVE validators (active-active) — e.g. the 2x2 topology is
# 2 validators + 2 RPC trackers in each DC. Each list's LENGTH sets that role's
# count; its VALUES set placement (repeat an IP to co-locate another process on
# that box). We assemble the positional NODE_IPS / BACKUP_SITE_NODE_IPS the rest
# of the tooling consumes (slot order: validators, spares, rpcs) AND export the
# per-role vars so reconcile/create-l1 read the counts directly. Validation
# (>=3 validators TOTAL across sites, >=1 rpc per site) lives in reconcile/loadPool.
#
# LEGACY fallback: if VALIDATOR_IPS is unset, NODE_IPS / BACKUP_SITE_NODE_IPS are
# used as-is (the fixed 3 validators + 1 spare + 2 RPCs layout).
if [ -n "${VALIDATOR_IPS:-}" ]; then
    NODE_IPS="${VALIDATOR_IPS}${SPARE_IPS:+,${SPARE_IPS}}${RPC_IPS:+,${RPC_IPS}}"
    if [ -n "${BACKUP_VALIDATOR_IPS:-}" ]; then
        BACKUP_SITE_NODE_IPS="${BACKUP_VALIDATOR_IPS}${BACKUP_SPARE_IPS:+,${BACKUP_SPARE_IPS}}${BACKUP_RPC_IPS:+,${BACKUP_RPC_IPS}}"
    fi
    export VALIDATOR_IPS SPARE_IPS RPC_IPS BACKUP_VALIDATOR_IPS BACKUP_SPARE_IPS BACKUP_RPC_IPS
    PER_ROLE_TOPOLOGY=1
fi

# Parse NODE_IPS into array
if [ -z "$NODE_IPS" ]; then
    echo "ERROR: no node IPs set in .env"
    echo ""
    echo "Set per-role lists (VALIDATOR_IPS / SPARE_IPS / RPC_IPS), or the legacy NODE_IPS."
    exit 1
fi

IFS=',' read -ra NODE_IPS_ARRAY <<< "$NODE_IPS"
NODE_COUNT=${#NODE_IPS_ARRAY[@]}

# Legacy positional mode requires exactly the fixed 6-slot site. Per-role mode is
# count-flexible (reconcile enforces >=3 validators / >=2 rpc).
if [ -z "${PER_ROLE_TOPOLOGY:-}" ] && [ "$NODE_COUNT" -ne 6 ]; then
    echo "ERROR: legacy NODE_IPS must contain exactly six node IPs (or use VALIDATOR_IPS/SPARE_IPS/RPC_IPS)"
    exit 1
fi

# Optional backup site (site B) for two-site failover.
BACKUP_SITE_NODE_IPS="${BACKUP_SITE_NODE_IPS:-}"
if [ -n "$BACKUP_SITE_NODE_IPS" ]; then
    IFS=',' read -ra BACKUP_SITE_IPS_ARRAY <<< "$BACKUP_SITE_NODE_IPS"
    if [ -z "${PER_ROLE_TOPOLOGY:-}" ] && [ "${#BACKUP_SITE_IPS_ARRAY[@]}" -ne 6 ]; then
        echo "ERROR: legacy BACKUP_SITE_NODE_IPS must contain exactly six backup-site node IPs"
        exit 1
    fi
fi

# First benchmark node is the default benchmark ingress host.
BOOTSTRAP_IP="${NODE_IPS_ARRAY[0]}"

SUBNET_EVM_ID="srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"
STAKING_DIR="$SCRIPT_DIR/staking"
NODE_IDS_FILE="$STAKING_DIR/node-ids.env"
PCHAIN_NODE_COUNT=5
PCHAIN_HTTP_BASE_PORT=9650
PCHAIN_STAKING_BASE_PORT=9651
PCHAIN_PORT_STEP=10

join_by_comma() {
    local IFS=,
    echo "$*"
}

node_id_for_l1_index() {
    local idx=$1
    if [ ! -f "$NODE_IDS_FILE" ]; then
        echo "ERROR: missing $NODE_IDS_FILE" >&2
        exit 1
    fi
    local value
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

pchain_node_ids_csv() {
    local ids=()
    local i
    for i in $(seq 1 "$PCHAIN_NODE_COUNT"); do
        ids+=("$(node_id_for_l1_index "$i")")
    done
    join_by_comma "${ids[@]}"
}

pchain_public_ip() {
    # .env may pin this (e.g. 127.0.0.1 for an all-loopback single-box run, where
    # every node advertises loopback and the curl'd NAT IP would be unreachable).
    [ -n "${PCHAIN_PUBLIC_IP:-}" ] && { echo "$PCHAIN_PUBLIC_IP"; return; }
    curl -fsS https://checkip.amazonaws.com | tr -d '[:space:]'
}

pchain_public_staking_ips_csv() {
    local public_ip=$1
    local ips=()
    local i
    for i in $(seq 1 "$PCHAIN_NODE_COUNT"); do
        ips+=("$public_ip:$(pchain_staking_port "$i")")
    done
    join_by_comma "${ips[@]}"
}

print_nodes() {
    for i in "${!NODE_IPS_ARRAY[@]}"; do
        local n=$((i + 1))
        echo "  Benchmark node $n: ${NODE_IPS_ARRAY[$i]}"
    done
}

# _count returns the number of comma-separated entries in its argument (0 if empty).
_count() {
    local s="${1:-}"
    [ -z "$s" ] && { echo 0; return; }
    local arr
    IFS=',' read -ra arr <<< "$s"
    echo "${#arr[@]}"
}

# ensure_staking_keys verifies every committed staking identity the configured
# topology will reference (staking/l1/6 .. 6+registered+Size-1, where registered
# is NVal per site across every site) actually exists. Keys are generated with
# `go run ./cmd/genstaking` LOCALLY and committed/shipped in the kit — the
# control box has no Go toolchain — so this is a pre-flight check with a clear
# remedy, not a generator. Mirrors reconcile's key scheme (plan.go).
ensure_staking_keys() {
    local nval nspare nrpc sp size nreg maxkey k
    if [ -n "${PER_ROLE_TOPOLOGY:-}" ]; then
        nval=$(_count "$VALIDATOR_IPS"); nspare=$(_count "$SPARE_IPS"); nrpc=$(_count "$RPC_IPS")
    else
        nval=3; nspare=1; nrpc=2
    fi
    sp=$((nval + nspare + nrpc))
    size=$sp
    nreg=$nval
    [ -n "$BACKUP_SITE_NODE_IPS" ] && { size=$((2 * sp)); nreg=$((2 * nval)); }
    maxkey=$((6 + nreg + size - 1))

    for k in $(seq 6 "$maxkey"); do
        if [ ! -d "$STAKING_DIR/l1/$k" ]; then
            echo "ERROR: topology (${nval}v/${nspare}s/${nrpc}r per site) needs committed staking key $k," >&2
            echo "       but $STAKING_DIR/l1/$k is missing. Generate locally and rebuild the kit:" >&2
            echo "         go run ./cmd/genstaking $k $maxkey >> staking/node-ids.env" >&2
            exit 1
        fi
    done
}
