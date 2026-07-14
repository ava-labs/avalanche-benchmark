#!/bin/bash
# Generate ALL per-deploy secrets into gitignored paths (KEY POLICY: nothing that
# touches the anchor network's public P-chain, or that identifies a validator
# on it, is ever committed):
#
#   staking/l1/<name>/{staker.crt,staker.key[,signer.key]}   one identity per
#       nodes.ini node (their NodeIDs get bound as validationIDs, so a leaked
#       staking key = validator impersonation); the BLS signer.key exists for
#       role=validator nodes ONLY - rpc identities never get one
#   staking/fuji-wallet.key                                fund/fee wallet +
#       L1 owner key (one secp256k1 key covers both roles for the benchmark)
#   staking/node-ids.env                                   NON-secret manifest
#       (<name>=<NodeID> lines) every other script/tool reads instead of
#       hardcoded values
#
# Refuses to overwrite anything: existing identities may be registered on the
# P-chain and the wallet may hold funds. For a truly fresh identity set, remove
# staking/ yourself first (and accept that the old chain is unreachable).
set -e
trap 'echo "ERROR: 00_gen_secrets failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$SCRIPT_DIR/_common.sh"

GENSTAKING="$SCRIPT_DIR/bin/genstaking"
FUJI_WALLET="$SCRIPT_DIR/bin/fuji-wallet"
for b in "$GENSTAKING" "$FUJI_WALLET"; do
    if [ ! -x "$b" ]; then
        echo "ERROR: $b not found. Build the kit first (make build)."
        exit 1
    fi
done

echo "=== Generate deploy secrets ==="
echo "One identity per nodes.ini node (gitignored, never commit)."
echo ""

# ------------------------------------------------------------------------------
# [1/2] Staking identities + NodeID manifest (from nodes.ini; generate-if-absent)
# ------------------------------------------------------------------------------
mkdir -p "$STAKING_DIR"
echo "[1/2] Staking identities..."
(cd "$SCRIPT_DIR" && "$GENSTAKING")

# ------------------------------------------------------------------------------
# [2/2] Fund/fee wallet (+ its addresses, for reference)
# ------------------------------------------------------------------------------
if [ -f "$FUJI_WALLET_KEY" ]; then
    echo "[2/2] $FUJI_WALLET_KEY already exists: keeping it (it may hold funds / own the L1)."
else
    echo "[2/2] Generating wallet..."
    "$FUJI_WALLET" gen -key "$FUJI_WALLET_KEY"
fi

echo ""
echo "=== Secrets ready ==="
echo "Manifest: $NODE_IDS_FILE"
echo ""
echo "Next step: ./setup/01_fund_wallet.sh"
