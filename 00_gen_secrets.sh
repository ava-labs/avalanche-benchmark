#!/bin/bash
# Generate ALL per-deploy secrets into gitignored paths (KEY POLICY: nothing that
# touches Fuji's public P-chain, or that identifies a validator on it, is ever
# committed):
#
#   staking/l1/<idx>/{staker.crt,staker.key,signer.key}   node identities the
#       configured topology needs (their NodeIDs get bound as validationIDs on
#       Fuji, so a leaked staking key = validator impersonation)
#   staking/fuji-wallet.key                                Fuji fund/fee wallet +
#       L1 owner key (one secp256k1 key covers both roles for the benchmark)
#   staking/node-ids.env                                   NON-secret manifest
#       (L1_<idx>_NODE_ID=...) every other script/tool reads instead of
#       hardcoded values
#
# Refuses to overwrite anything: existing identities may be registered on Fuji
# and the wallet may hold funds. For a truly fresh identity set, remove
# staking/ yourself first (and accept that the old chain is unreachable).
set -e
trap 'echo "ERROR: 00 failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"

GENSTAKING="$SCRIPT_DIR/bin/genstaking"
FUJI_WALLET="$SCRIPT_DIR/bin/fuji-wallet"
for b in "$GENSTAKING" "$FUJI_WALLET"; do
    if [ ! -x "$b" ]; then
        echo "ERROR: $b not found. Build the kit first (make build)."
        exit 1
    fi
done

MAX_KEY=$(staking_max_key)
echo "=== Generate deploy secrets ==="
echo "Topology needs staking keys $L1_VALIDATOR_START_INDEX..$MAX_KEY (gitignored, never commit)."
echo ""

# ------------------------------------------------------------------------------
# [1/2] Staking identities + NodeID manifest
# ------------------------------------------------------------------------------
mkdir -p "$STAKING_DIR"
missing=()
for k in $(seq "$L1_VALIDATOR_START_INDEX" "$MAX_KEY"); do
    [ -d "$STAKING_DIR/l1/$k" ] || missing+=("$k")
done

if [ "${#missing[@]}" -eq 0 ]; then
    echo "[1/2] Staking identities $L1_VALIDATOR_START_INDEX..$MAX_KEY already exist: keeping them."
else
    first="${missing[0]}"
    last="${missing[${#missing[@]}-1]}"
    if [ "$((last - first + 1))" -ne "${#missing[@]}" ]; then
        echo "ERROR: staking/l1 has holes (missing: ${missing[*]}). Refusing to guess;"
        echo "       remove staking/ for a fresh set or fill the gap manually."
        exit 1
    fi
    echo "[1/2] Generating staking identities $first..$last..."
    # genstaking writes staking/l1/<idx>/ relative to the cwd and prints the
    # manifest lines; append them so an extended topology keeps existing IDs.
    (cd "$SCRIPT_DIR" && "$GENSTAKING" "$first" "$last" >> "$NODE_IDS_FILE")
fi

# ------------------------------------------------------------------------------
# [2/2] Fuji fund/fee wallet (+ its addresses, for reference)
# ------------------------------------------------------------------------------
if [ -f "$FUJI_WALLET_KEY" ]; then
    echo "[2/2] $FUJI_WALLET_KEY already exists: keeping it (it may hold funds / own the L1)."
else
    echo "[2/2] Generating Fuji wallet..."
    "$FUJI_WALLET" gen -key "$FUJI_WALLET_KEY"
fi

echo ""
echo "=== Secrets ready ==="
echo "Manifest: $NODE_IDS_FILE"
echo ""
echo "Next step: ./01_fund_wallet.sh   (manual Fuji faucet -> auto C->P move)"
