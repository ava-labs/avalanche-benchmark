#!/bin/bash
# Common configuration loader for remote benchmark scripts.
# Source this from other scripts after setting SCRIPT_DIR:
#
#   SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#   source "$SCRIPT_DIR/_common.sh"

ENV_FILE="$SCRIPT_DIR/.env"
NETWORK_ENV="$SCRIPT_DIR/network.env"
REMOTE_DIR="~/avalanche-benchmark"
SSH_KEY_PATH_DEFAULT="/home/ubuntu/.ssh/ilya-solohin-failover-bench-2026-05-04"

# Load .env
if [ ! -f "$ENV_FILE" ]; then
    echo "ERROR: .env file not found"
    echo ""
    echo "Create .env from the template:"
    echo "  cp .env.example .env"
    exit 1
fi

source "$ENV_FILE"

# The fleet inventory: nodes.ini in the repo root (one line per node:
# `<name> host=<ip> role=validator|rpc [dc=<tag>]`). The Go
# tools parse it themselves (internal/topo); shell scripts get per-node rows
# from `./fleet endpoints` (name, dc, role, host, port).
if [ ! -f "$SCRIPT_DIR/nodes.ini" ]; then
    echo "ERROR: nodes.ini not found (the fleet inventory)."
    echo "       Create it in the repo root; see the shipped nodes.ini for the format."
    exit 1
fi

# Network the L1 anchors on. NETWORK in network.env (a property of the created
# chain, persisted by create-l1) always wins; AVALANCHE_NETWORK from the shell
# or .env is the pre-creation input (the setup scripts' --mainnet flag exports
# it); default fuji. Exported so the Go tools resolve the same network.
if [ -f "$NETWORK_ENV" ]; then
    _network_env_net="$(sed -n 's/^NETWORK=//p' "$NETWORK_ENV" | tail -n1)"
    [ -n "$_network_env_net" ] && AVALANCHE_NETWORK="$_network_env_net"
fi
AVALANCHE_NETWORK="${AVALANCHE_NETWORK:-fuji}"
export AVALANCHE_NETWORK

# Export REMOTE_DIR so the reconcile child inherits the SAME deploy dir the shell
# scripts use. Lets .env point the deploy at a dir OTHER than the repo: required
# for a localhost run (REMOTE_DIR=repo would scp binaries onto themselves).
export REMOTE_DIR

SSH_KEY_PATH="${SSH_KEY_PATH:-$SSH_KEY_PATH_DEFAULT}"
SSH_OPTS=(
    -i "$SSH_KEY_PATH"
    -o IdentitiesOnly=yes
    -o IdentityAgent=none
    -o UserKnownHostsFile=/dev/null
    -o StrictHostKeyChecking=no
    -o ControlMaster=no
    -o ControlPath=none
    -o LogLevel=ERROR
    # Fail fast on a wedged host instead of hanging forever: bound the
    # connect/banner phase, and bail (~60s) if an established connection goes
    # dead mid-command. Kept in sync with cmd/reconcile/remote.go sshArgs.
    -o ConnectTimeout=10
    -o ServerAliveInterval=15
    -o ServerAliveCountMax=4
)

ssh() {
    command ssh "${SSH_OPTS[@]}" "$@"
}

scp() {
    command scp "${SSH_OPTS[@]}" "$@"
}

# Validate SSH_USER
if [ -z "$SSH_USER" ]; then
    echo "ERROR: SSH_USER not set in .env"
    exit 1
fi

SUBNET_EVM_ID="srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"
STAKING_DIR="$SCRIPT_DIR/staking"
NODE_IDS_FILE="$STAKING_DIR/node-ids.env"
FUJI_WALLET_KEY="$STAKING_DIR/fuji-wallet.key"

# Public peer the RPC tier's P-chain follows: the fleet's ONE allowed
# outgoing TCP. Default: the first entry for AVALANCHE_NETWORK in the pinned
# avalanchego commit's genesis/bootstrappers.json (Ava Labs-operated; the
# NodeID is enforced by the TLS handshake, so a hijacked IP cannot impersonate
# it). RUNBOOK: these hardcoded IPs rotate between releases: on every
# AVALANCHEGO_COMMIT bump, re-check bootstrappers.json and update these
# defaults (and internal/netcfg), the SG egress rule, and any .env override
# TOGETHER. A stale IP fails closed (P-chain feed freezes; the L1 keeps
# mining). The FUJI_UPSTREAM_* names are kept on both networks for .env
# compatibility.
if [ "$AVALANCHE_NETWORK" = "mainnet" ]; then
    FUJI_UPSTREAM_IPS="${FUJI_UPSTREAM_IPS:-54.232.137.108:9651}"
    FUJI_UPSTREAM_IDS="${FUJI_UPSTREAM_IDS:-NodeID-A6onFGyJjA37EZ7kYHANMR1PFRT8NmXrF}"
else
    FUJI_UPSTREAM_IPS="${FUJI_UPSTREAM_IPS:-18.192.93.241:9651}"
    FUJI_UPSTREAM_IDS="${FUJI_UPSTREAM_IDS:-NodeID-2m38qc95mhHXtrhjyGbe7r2NhniqHHJRB}"
fi
export FUJI_UPSTREAM_IPS FUJI_UPSTREAM_IDS
