#!/bin/bash
# Shared setup for the failover wrappers and 03. Sourced after the caller sets
# REPO_DIR to the repo root. It loads .env + network.env via the repo's
# _common.sh and exports everything the `reconcile` binary reads from the
# environment (reconcile derives the per-role P-chain beacon and sibling-seed
# lists itself, from the topology + staking/node-ids.env).
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
    echo "ERROR: network.env not found. Run ./02_create_chain.sh first." >&2
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

export NODE_IPS BACKUP_SITE_NODE_IPS SSH_USER SSH_KEY_PATH REMOTE_DIR
export CHAIN_ID SUBNET_ID SUBNET_EVM_ID
export FUJI_UPSTREAM_IPS FUJI_UPSTREAM_IDS
export REPO_DIR
export FAILOVER_STATE_FILE="$REPO_DIR/scripts/failover/intentions.json"
# MANAGER_ADDRESS (from network.env) enables the on-chain weight reconciliation;
# PCHAIN_API (optional .env override) is where reconcile reaches Fuji's public
# P-chain + C-chain APIs.
export MANAGER_ADDRESS PCHAIN_API
