#!/bin/bash
# Bundle everything secret and unrecoverable into one tar.gz: staking/ (all
# validator identities + the wallet key) and network.env (the chain identity).
# Losing these means losing control of the chain; neither is in git or in any
# kit archive. This is YOUR OWN off-machine backup, not a handover bundle: the
# only secret ever handed to anyone is the single wallet key, everything else
# here is regenerated on a fresh run. To rebuild a control host, untar this
# over a fresh kit root and resume (no re-creation, no re-spend).
set -e
trap 'echo "ERROR: 03_backup_secrets failed at line $LINENO. Command: $BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ ! -d "$SCRIPT_DIR/staking" ]; then
    echo "ERROR: $SCRIPT_DIR/staking not found. Run ./setup/00_gen_secrets.sh first."
    exit 1
fi

FILES=(staking)
if [ -f "$SCRIPT_DIR/network.env" ]; then
    FILES+=(network.env)
else
    echo "WARNING: network.env not found (no chain created yet); backing up staking/ only."
fi

OUT="${1:-$SCRIPT_DIR/secrets-backup-$(date +%Y%m%d-%H%M%S).tar.gz}"
tar -czf "$OUT" -C "$SCRIPT_DIR" "${FILES[@]}"

echo "Backed up ${FILES[*]} -> $OUT ($(du -h "$OUT" | cut -f1))"
echo "Restore: tar -xzf $(basename "$OUT") -C <kit root>"
