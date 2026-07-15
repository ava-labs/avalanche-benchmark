#!/bin/bash
# CREATE the chain via `bin/l1 create`, issued against the PUBLIC API (our own
# RPC tier is follow-only, so its platform.* API is gated forever). This builds
# TWO L1s from the one wallet key: a small MANAGER L1 whose equal-weight
# validators are a signing COMMITTEE (default 4), and the MAIN L1 (the fleet)
# whose recorded validator manager is that committee's chain. We hold every
# committee BLS key, so all later weight changes are self-signed locally by
# `bin/l1` with no contract, courier or aggregator. This SPENDS AVAX (fees +
# the per-validator continuous-fee balances of BOTH L1s; keep the committee
# funded) and registers the generated NodeIDs on a public chain, so run it ONCE
# per chain. Re-deploys of the fleet go through ./run/01_deploy.sh, which never
# re-creates (and never re-spends).
set -e

# Error handler to show what went wrong
trap 'echo "ERROR: Script failed at line $LINENO. Command: $BASH_COMMAND"' ERR

# --mainnet creates the L1 anchored on Avalanche mainnet (REAL AVAX). The
# choice is persisted as NETWORK in network.env by l1 create; on resume that
# record wins and a conflicting flag is rejected below. Any other flag is
# forwarded verbatim to `bin/l1 create`, so -balance, -committee,
# -committee-balance and -allow-fragile-committee all work through here.
REQUESTED_NETWORK=""
CREATE_ARGS=()
for arg in "$@"; do
    case "$arg" in
        --mainnet) REQUESTED_NETWORK=mainnet; export AVALANCHE_NETWORK=mainnet ;;
        -h|--help) echo "usage: $0 [--mainnet] [-balance AVAX] [-committee N] [-committee-balance AVAX] [-allow-fragile-committee]"; exit 0 ;;
        *) CREATE_ARGS+=("$arg") ;;
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
# Resume semantics: l1 create persists every step's result to network.env as it
# completes and SKIPS anything already present. It refuses to run over an
# existing SUBNET_ID unless forced, so the resume flag is passed explicitly
# here: re-running this script never creates a second chain or double-spends,
# it resumes/verifies the recorded one. To force a genuinely NEW L1, delete
# network.env first (the old chain becomes unreachable).
# ------------------------------------------------------------------------------
FORCE=""
if [ -f "$NETWORK_ENV" ] && grep -q '^SUBNET_ID=' "$NETWORK_ENV"; then
    source "$NETWORK_ENV"
    echo "network.env exists - resuming/verifying the recorded L1 (no new creation):"
    echo "  Subnet ID: ${SUBNET_ID:-<pending>}"
    echo "  Chain ID:  ${CHAIN_ID:-<pending>}"
    echo ""
    FORCE="--force"
fi

# ------------------------------------------------------------------------------
# Create L1 (subnet + chain + convert)
# ------------------------------------------------------------------------------
echo "=== Creating L1 on $AVALANCHE_NETWORK ==="
echo ""

"$SCRIPT_DIR/bin/l1" create $FORCE "${CREATE_ARGS[@]}"

# Load and display results
source "$NETWORK_ENV"

echo ""
echo "=== L1 Created ==="
echo ""
echo "Subnet ID:        $SUBNET_ID"
echo "Chain ID:         $CHAIN_ID"
echo "Manager subnet:   ${MANAGER_SUBNET_ID:-<self-managed>}"
echo "Manager chain:    ${MANAGER_CHAIN_ID:-<self-managed>} (its committee signs weight moves via bin/l1)"
echo "Manager address:  $MANAGER_ADDRESS"
echo ""
echo "Saved to: $NETWORK_ENV"
echo ""
echo "Next step: ./setup/03_backup_secrets.sh   (bundle staking/ + network.env off this machine)"
echo "Then:      ./run/01_deploy.sh   (deploy chain config and start the remote L1 validators)"
