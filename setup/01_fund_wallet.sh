#!/bin/bash
# Fund the generated Fuji wallet. Funding is MANUAL and C-CHAIN ONLY (there is
# no P-chain faucet): this prints the wallet's C-chain address, waits for you to
# hit the Fuji faucet, then AUTOMATICALLY moves everything C -> P (atomic export
# + import), so the P-chain wallet ends up funded with zero extra steps.
#
# Re-runnable: any new C-chain funds get swept to P; if the P balance is already
# sufficient just Ctrl-C at the prompt.
set -e
trap 'echo "ERROR: 01_fund_wallet failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$SCRIPT_DIR/_common.sh"

FUJI_WALLET="$SCRIPT_DIR/bin/fuji-wallet"
if [ ! -x "$FUJI_WALLET" ]; then
    echo "ERROR: $FUJI_WALLET not found. Build the kit first (make build)."
    exit 1
fi
if [ ! -f "$FUJI_WALLET_KEY" ]; then
    echo "ERROR: $FUJI_WALLET_KEY not found. Run ./setup/00_gen_secrets.sh first."
    exit 1
fi

# PCHAIN_API from .env overrides the default public Fuji API inside fuji-wallet.
"$FUJI_WALLET" fund -key "$FUJI_WALLET_KEY"

echo ""
echo "Next step: ./setup/02_create_chain.sh   (creates the L1 on Fuji, SPENDS AVAX)"
