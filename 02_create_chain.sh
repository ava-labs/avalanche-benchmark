#!/bin/bash
# CREATE the chain on Fuji: subnet + chain + ConvertSubnetToL1Tx, issued against
# the PUBLIC Fuji API (our own RPC tier is follow-only, so its platform.* API is
# gated forever). This SPENDS AVAX (fees + 1 AVAX continuous-fee balance per
# validator) and registers the generated NodeIDs on a public chain, so run it
# ONCE per chain. Re-deploys of the fleet go through ./03_deploy_chain.sh, which
# never re-creates (and never re-spends).
set -e

# Error handler to show what went wrong
trap 'echo "ERROR: Script failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"

# ------------------------------------------------------------------------------
# Check if L1 already exists
# ------------------------------------------------------------------------------
if [ -f "$NETWORK_ENV" ]; then
    source "$NETWORK_ENV"
    echo "WARNING: network.env already exists"
    echo "  Subnet ID: $SUBNET_ID"
    echo "  Chain ID:  $CHAIN_ID"
    echo ""
    echo "Creating a NEW chain costs AVAX and abandons the one above (the fleet"
    echo "can keep using it via ./03_deploy_chain.sh, no new creation needed)."
    read -p "Create a NEW L1 anyway? This will overwrite network.env. [y/N] " -n 1 -r
    echo ""
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "Aborted. To (re)deploy the fleet against the existing L1, run: ./03_deploy_chain.sh"
        exit 0
    fi
fi

# Pre-flight: every generated staking identity the configured topology references
# must exist (created by ./00_gen_secrets.sh, never committed).
ensure_staking_keys

# ------------------------------------------------------------------------------
# Create L1 (subnet + chain + convert) on Fuji
# ------------------------------------------------------------------------------
echo "=== Creating L1 on Fuji ==="
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
echo "Next step: ./03_deploy_chain.sh"
echo "  This will deploy chain config and start remote L1 validators."
