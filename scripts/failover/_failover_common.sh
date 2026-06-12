#!/bin/bash
# Shared setup for the failover wrappers and 03. Sourced after the caller sets
# REPO_DIR to the repo root. It loads .env + network.env via the repo's
# _common.sh, computes the static P-chain bootstrap set, and exports everything
# the `reconcile` binary reads from the environment.
#
# The reconcile binary owns all SSH/scp I/O itself (it replicates the YubiKey-safe
# SSH options from _common.sh); this layer only assembles config.

if [ -z "$REPO_DIR" ]; then
    echo "ERROR: _failover_common.sh requires REPO_DIR set by the caller" >&2
    exit 1
fi

# _common.sh keys its file lookups off SCRIPT_DIR; point it at the repo root.
SCRIPT_DIR="$REPO_DIR"
source "$REPO_DIR/_common.sh"

if [ ! -f "$NETWORK_ENV" ]; then
    echo "ERROR: network.env not found. Run ./02_create_l1.sh first." >&2
    exit 1
fi
source "$NETWORK_ENV"

if [ -z "$SUBNET_ID" ] || [ -z "$CHAIN_ID" ]; then
    echo "ERROR: SUBNET_ID or CHAIN_ID not set in network.env" >&2
    exit 1
fi

RECONCILE_BIN="$REPO_DIR/bin/reconcile"
if [ ! -x "$RECONCILE_BIN" ]; then
    echo "ERROR: $RECONCILE_BIN not found. Run 'make build' first." >&2
    exit 1
fi

# Static P-chain bootstrap set (control machine's public IP + the 5 P-chain NodeIDs).
PCHAIN_PUBLIC_IP="$(pchain_public_ip)"

export NODE_IPS BACKUP_SITE_NODE_IPS SSH_USER SSH_KEY_PATH REMOTE_DIR
export CHAIN_ID SUBNET_ID SUBNET_EVM_ID
export PCHAIN_BOOTSTRAP_IPS="$(pchain_public_staking_ips_csv "$PCHAIN_PUBLIC_IP")"
export PCHAIN_BOOTSTRAP_IDS="$(pchain_node_ids_csv)"
export REPO_DIR
export FAILOVER_STATE_FILE="$REPO_DIR/scripts/failover/intentions.json"
