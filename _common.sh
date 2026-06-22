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

# Parse NODE_IPS into array
if [ -z "$NODE_IPS" ]; then
    echo "ERROR: NODE_IPS not set in .env"
    echo ""
    echo "Set NODE_IPS to exactly six comma-separated benchmark node IPs."
    exit 1
fi

IFS=',' read -ra NODE_IPS_ARRAY <<< "$NODE_IPS"
NODE_COUNT=${#NODE_IPS_ARRAY[@]}

if [ "$NODE_COUNT" -ne 6 ]; then
    echo "ERROR: NODE_IPS must contain exactly six benchmark node IPs"
    exit 1
fi

# Optional backup site (site B) for two-site failover. When set it must be
# exactly six more IPs: b1-b3 zero-weight syncing trackers, b4 spare, b5/b6 archive RPCs.
BACKUP_SITE_NODE_IPS="${BACKUP_SITE_NODE_IPS:-}"
if [ -n "$BACKUP_SITE_NODE_IPS" ]; then
    IFS=',' read -ra BACKUP_SITE_IPS_ARRAY <<< "$BACKUP_SITE_NODE_IPS"
    if [ "${#BACKUP_SITE_IPS_ARRAY[@]}" -ne 6 ]; then
        echo "ERROR: BACKUP_SITE_NODE_IPS must contain exactly six backup-site node IPs"
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
L1_VALIDATOR_START_INDEX=6
L1_VALIDATOR_COUNT=3

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
