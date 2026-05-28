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
    echo "Examples:"
    echo "  NODE_IPS=1.2.3.1               (single validator)"
    echo "  NODE_IPS=1.2.3.1,1.2.3.2,1.2.3.3  (three validators)"
    exit 1
fi

IFS=',' read -ra NODE_IPS_ARRAY <<< "$NODE_IPS"
NODE_COUNT=${#NODE_IPS_ARRAY[@]}

if [ "$NODE_COUNT" -lt 1 ]; then
    echo "ERROR: NODE_IPS must contain at least one IP"
    exit 1
fi

# First node is always the bootstrap and monitoring host
BOOTSTRAP_IP="${NODE_IPS_ARRAY[0]}"

SUBNET_EVM_ID="srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"
STAKING_DIR="$SCRIPT_DIR/staking"
NODE_IDS_FILE="$STAKING_DIR/node-ids.env"
SYBIL_ENABLED_LOCAL="${SYBIL_ENABLED_LOCAL:-false}"
L1_VALIDATOR_START_INDEX="${L1_VALIDATOR_START_INDEX:-}"
L1_VALIDATOR_COUNT="${L1_VALIDATOR_COUNT:-}"

is_truthy() {
    case "$1" in
        1|true|TRUE|yes|YES|y|Y|on|ON) return 0 ;;
        *) return 1 ;;
    esac
}

if is_truthy "$SYBIL_ENABLED_LOCAL"; then
    if [ -z "$L1_VALIDATOR_START_INDEX" ]; then
        L1_VALIDATOR_START_INDEX=6
    fi
    if [ -z "$L1_VALIDATOR_COUNT" ]; then
        L1_VALIDATOR_COUNT=5
    fi
fi

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

print_nodes() {
    for i in "${!NODE_IPS_ARRAY[@]}"; do
        local n=$((i + 1))
        local label=""
        if [ "$i" -eq 0 ]; then label=" (bootstrap)"; fi
        echo "  Node $n: ${NODE_IPS_ARRAY[$i]}$label"
    done
}
