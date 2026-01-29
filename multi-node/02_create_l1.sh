#!/bin/bash
set -e

# Error handler to show what went wrong
trap 'echo "ERROR: Script failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"
NETWORK_ENV="$SCRIPT_DIR/network.env"

# ------------------------------------------------------------------------------
# Load configuration
# ------------------------------------------------------------------------------
if [ ! -f "$ENV_FILE" ]; then
    echo "ERROR: .env file not found"
    exit 1
fi

source "$ENV_FILE"

if [ -z "$NODE1_IP" ]; then
    echo "ERROR: NODE1_IP not set in .env"
    exit 1
fi

# ------------------------------------------------------------------------------
# Check if L1 already exists
# ------------------------------------------------------------------------------
if [ -f "$NETWORK_ENV" ]; then
    source "$NETWORK_ENV"
    echo "WARNING: network.env already exists"
    echo "  Subnet ID: $SUBNET_ID"
    echo "  Chain ID:  $CHAIN_ID"
    echo ""
    read -p "Create a NEW L1? This will overwrite network.env. [y/N] " -n 1 -r
    echo ""
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "Aborted. To deploy config to existing L1, run: ./03_deploy_l1_config.sh"
        exit 0
    fi
fi

# ------------------------------------------------------------------------------
# Create L1 (subnet + chain + convert)
# ------------------------------------------------------------------------------
echo "=== Creating L1 ==="
echo ""

"$SCRIPT_DIR/bin/create-l1" -output "$NETWORK_ENV"

# Load and display results
source "$NETWORK_ENV"

echo ""
echo "=== L1 Created ==="
echo ""
echo "Subnet ID: $SUBNET_ID"
echo "Chain ID:  $CHAIN_ID"
echo ""
echo "Saved to: $NETWORK_ENV"
echo ""
echo "Next step: ./03_deploy_l1_config.sh"
echo "  This will deploy chain configs and start validator/RPC nodes."
