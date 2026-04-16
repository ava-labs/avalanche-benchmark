#!/bin/bash
# Common configuration loader for multi-node benchmark scripts.
# Source this from other scripts after setting SCRIPT_DIR:
#
#   SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#   source "$SCRIPT_DIR/_common.sh"

ENV_FILE="$SCRIPT_DIR/.env"
NETWORK_ENV="$SCRIPT_DIR/network.env"
REMOTE_DIR="~/avalanche-benchmark"

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

print_nodes() {
    for i in "${!NODE_IPS_ARRAY[@]}"; do
        local n=$((i + 1))
        local label=""
        if [ "$i" -eq 0 ]; then label=" (bootstrap)"; fi
        echo "  Node $n: ${NODE_IPS_ARRAY[$i]}$label"
    done
}
