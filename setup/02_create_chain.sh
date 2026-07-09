#!/bin/bash
# CREATE the chain on Fuji: subnet + chain + ConvertSubnetToL1Tx, issued against
# the PUBLIC Fuji API (our own RPC tier is follow-only, so its platform.* API is
# gated forever). This SPENDS AVAX (fees + 0.1 AVAX continuous-fee balance per
# validator) and registers the generated NodeIDs on a public chain, so run it
# ONCE per chain. Re-deploys of the fleet go through ./run/01_deploy.sh, which
# never re-creates (and never re-spends).
set -e

# Error handler to show what went wrong
trap 'echo "ERROR: Script failed at line $LINENO. Command: $BASH_COMMAND"' ERR

# --mainnet creates the L1 anchored on Avalanche mainnet (REAL AVAX). The
# choice is persisted as NETWORK in network.env by create-l1; on resume that
# record wins and a conflicting flag is rejected below.
REQUESTED_NETWORK=""
for arg in "$@"; do
    case "$arg" in
        --mainnet) REQUESTED_NETWORK=mainnet; export AVALANCHE_NETWORK=mainnet ;;
        *) echo "usage: $0 [--mainnet]"; exit 2 ;;
    esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$SCRIPT_DIR/_common.sh"

# _common.sh resolved AVALANCHE_NETWORK (network.env NETWORK wins). A flag that
# contradicts an already-created chain is an operator error, not a migration.
if [ -n "$REQUESTED_NETWORK" ] && [ "$REQUESTED_NETWORK" != "$AVALANCHE_NETWORK" ]; then
    echo "ERROR: --$REQUESTED_NETWORK requested but network.env records NETWORK=$AVALANCHE_NETWORK."
    echo "       Delete network.env to create a NEW chain on another network."
    exit 1
fi

# ------------------------------------------------------------------------------
# Resume semantics: create-l1 persists every step's result to network.env as it
# completes and SKIPS anything already present, so re-running here never creates
# a second chain or double-spends - it resumes/verifies the recorded one
# (including finishing a failed initializeValidatorSet). To force a genuinely
# NEW L1, delete network.env first (the old chain becomes unreachable).
# ------------------------------------------------------------------------------
if [ -f "$NETWORK_ENV" ]; then
    source "$NETWORK_ENV"
    echo "network.env exists - resuming/verifying the recorded L1 (no new creation):"
    echo "  Subnet ID: ${SUBNET_ID:-<pending>}"
    echo "  Chain ID:  ${CHAIN_ID:-<pending>}"
    echo "  Manager:   ${MANAGER_ADDRESS:-<pending>}"
    echo ""
fi

# Pre-flight: every generated staking identity the configured topology references
# must exist (created by ./setup/00_gen_secrets.sh, never committed).
ensure_staking_keys

# ------------------------------------------------------------------------------
# Create L1 (subnet + chain + convert) on Fuji
# ------------------------------------------------------------------------------
echo "=== Creating L1 on $AVALANCHE_NETWORK ==="
echo ""

"$SCRIPT_DIR/bin/create-l1" -output "$NETWORK_ENV"

# Load and display results
source "$NETWORK_ENV"

echo ""
echo "=== L1 Created ==="
echo ""
echo "Subnet ID: $SUBNET_ID"
echo "Chain ID:  $CHAIN_ID"
echo "Manager:   $MANAGER_ADDRESS (ValidatorManager on the Fuji C-chain)"
echo ""
echo "Saved to: $NETWORK_ENV"
echo ""
echo "Next step: ./setup/03_backup_secrets.sh   (bundle staking/ + network.env off this machine)"
echo "Then:      ./run/01_deploy.sh   (deploy chain config and start the remote L1 validators)"
