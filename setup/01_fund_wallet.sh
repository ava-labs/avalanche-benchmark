#!/bin/bash
# Fund the generated Fuji wallet. Funding is MANUAL: this prints the wallet's
# P-chain and C-chain addresses with the required amounts, then polls both
# balances until each chain is funded at the Fuji faucet. No cross-chain
# moves: fund each chain directly.
#
# Re-runnable: exits immediately if both balances are already sufficient.
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
