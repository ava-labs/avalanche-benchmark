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

# Network the L1 anchors on. NETWORK in network.env (a property of the created
# chain, persisted by create-l1) always wins; AVALANCHE_NETWORK from the shell
# or .env is the pre-creation input (the setup scripts' --mainnet flag exports
# it); default fuji. Exported so the Go tools resolve the same network.
if [ -f "$NETWORK_ENV" ]; then
    _network_env_net="$(sed -n 's/^NETWORK=//p' "$NETWORK_ENV" | tail -n1)"
    [ -n "$_network_env_net" ] && AVALANCHE_NETWORK="$_network_env_net"
fi
AVALANCHE_NETWORK="${AVALANCHE_NETWORK:-fuji}"
export AVALANCHE_NETWORK

# Export REMOTE_DIR so the reconcile child inherits the SAME deploy dir the shell
# scripts use. Lets .env point the deploy at a dir OTHER than the repo: required
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

# Topology config: explicit per-role IP lists per data center (REQUIRED; the
# legacy positional NODE_IPS format was removed with the C-chain managed-weights
# rework):
#   VALIDATOR_IPS / SPARE_IPS / RPC_IPS           (site A)
#   BACKUP_VALIDATOR_IPS / BACKUP_SPARE_IPS / BACKUP_RPC_IPS  (site B, optional)
# Each list's LENGTH sets that role's count; its VALUES set placement (repeat an IP
# to co-locate another process on that box). We assemble the positional NODE_IPS /
# BACKUP_SITE_NODE_IPS a few bash consumers still use (slot order: validators,
# spares, rpcs) AND export the per-role vars so the Go tools read the counts
# directly (validation lives in internal/topo.FromEnv).
if [ -z "${VALIDATOR_IPS:-}" ]; then
    echo "ERROR: VALIDATOR_IPS not set in .env (per-role lists are required:"
    echo "       VALIDATOR_IPS / SPARE_IPS / RPC_IPS, plus BACKUP_* for site B)"
    exit 1
fi
NODE_IPS="${VALIDATOR_IPS}${SPARE_IPS:+,${SPARE_IPS}}${RPC_IPS:+,${RPC_IPS}}"
if [ -n "${BACKUP_VALIDATOR_IPS:-}" ]; then
    BACKUP_SITE_NODE_IPS="${BACKUP_VALIDATOR_IPS}${BACKUP_SPARE_IPS:+,${BACKUP_SPARE_IPS}}${BACKUP_RPC_IPS:+,${BACKUP_RPC_IPS}}"
fi
export VALIDATOR_IPS SPARE_IPS RPC_IPS BACKUP_VALIDATOR_IPS BACKUP_SPARE_IPS BACKUP_RPC_IPS

IFS=',' read -ra NODE_IPS_ARRAY <<< "$NODE_IPS"
NODE_COUNT=${#NODE_IPS_ARRAY[@]}

# Optional backup site (site B) for two-site failover.
BACKUP_SITE_NODE_IPS="${BACKUP_SITE_NODE_IPS:-}"

# First benchmark node is the default benchmark ingress host.
BOOTSTRAP_IP="${NODE_IPS_ARRAY[0]}"

SUBNET_EVM_ID="srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"
STAKING_DIR="$SCRIPT_DIR/staking"
NODE_IDS_FILE="$STAKING_DIR/node-ids.env"
FUJI_WALLET_KEY="$STAKING_DIR/fuji-wallet.key"
L1_VALIDATOR_START_INDEX=1

# Public peer the RPC tier's P-chain follows: the fleet's ONE allowed
# outgoing TCP. Default: the first entry for AVALANCHE_NETWORK in the pinned
# avalanchego commit's genesis/bootstrappers.json (Ava Labs-operated; the
# NodeID is enforced by the TLS handshake, so a hijacked IP cannot impersonate
# it). RUNBOOK: these hardcoded IPs rotate between releases: on every
# AVALANCHEGO_COMMIT bump, re-check bootstrappers.json and update these
# defaults (and internal/netcfg), the SG egress rule, and any .env override
# TOGETHER. A stale IP fails closed (P-chain feed freezes; the L1 keeps
# mining). The FUJI_UPSTREAM_* names are kept on both networks for .env
# compatibility.
if [ "$AVALANCHE_NETWORK" = "mainnet" ]; then
    FUJI_UPSTREAM_IPS="${FUJI_UPSTREAM_IPS:-54.232.137.108:9651}"
    FUJI_UPSTREAM_IDS="${FUJI_UPSTREAM_IDS:-NodeID-A6onFGyJjA37EZ7kYHANMR1PFRT8NmXrF}"
else
    FUJI_UPSTREAM_IPS="${FUJI_UPSTREAM_IPS:-18.192.93.241:9651}"
    FUJI_UPSTREAM_IDS="${FUJI_UPSTREAM_IDS:-NodeID-2m38qc95mhHXtrhjyGbe7r2NhniqHHJRB}"
fi
export FUJI_UPSTREAM_IPS FUJI_UPSTREAM_IDS

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

# staking_max_key computes the highest committed key index the configured
# topology references. ONE permanent identity per pool slot: keys 1..Size
# (staking slots wear 1..N, RPC slots the rest; mirrors internal/topo KeyOf;
# identities never move between machines). Used by 00_gen_secrets.sh
# (generator) and ensure_staking_keys (pre-flight).
staking_max_key() {
    local nval nspare nrpc sp size
    nval=$(_count "$VALIDATOR_IPS"); nspare=$(_count "$SPARE_IPS"); nrpc=$(_count "$RPC_IPS")
    sp=$((nval + nspare + nrpc))
    size=$sp
    [ -n "$BACKUP_SITE_NODE_IPS" ] && size=$((2 * sp))
    echo $((L1_VALIDATOR_START_INDEX + size - 1))
}

# ensure_staking_keys verifies every GENERATED staking identity the configured
# topology will reference (staking/l1/1 .. staking_max_key) actually exists.
# Keys are generated per deploy by ./setup/00_gen_secrets.sh and are NEVER committed
# (their NodeIDs get bound as validationIDs on Fuji's public P-chain; a leaked
# staking key = validator impersonation). Pre-flight check, not a generator.
ensure_staking_keys() {
    local maxkey k
    maxkey=$(staking_max_key)
    for k in $(seq "$L1_VALIDATOR_START_INDEX" "$maxkey"); do
        if [ ! -d "$STAKING_DIR/l1/$k" ]; then
            echo "ERROR: the configured topology needs staking key $k, but" >&2
            echo "       $STAKING_DIR/l1/$k is missing. Generate the deploy secrets first:" >&2
            echo "         ./setup/00_gen_secrets.sh" >&2
            exit 1
        fi
    done
}
