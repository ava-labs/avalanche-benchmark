#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# Source tree: scripts/ and blockscout/ live as siblings of remote/.
# Offline pack: scripts/ and blockscout/ live alongside this wrapper.
if [[ -d "${SCRIPT_DIR}/scripts" && -f "${SCRIPT_DIR}/blockscout/docker-compose.yml" ]]; then
  ROOT_DIR="${SCRIPT_DIR}"
else
  ROOT_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
fi

if [[ ! -f "${ROOT_DIR}/scripts/blockscout-up.sh" ]]; then
  echo "Blockscout helpers not found under ${ROOT_DIR}." >&2
  echo "If you extracted remote-benchmark.tar.gz, rebuild the bundle with the explorer:" >&2
  echo "  cd remote && make pack-blockscout" >&2
  exit 1
fi

ACTION="up"
if [[ $# -gt 0 ]]; then
  case "$1" in
    -h|--help)
      echo "usage: ./blockscout.sh [up|smoke|down]" >&2
      exit 0
      ;;
    up|smoke|down)
      ACTION="$1"
      shift
      ;;
  esac
fi

case "${ACTION}" in
  up)
    source "${SCRIPT_DIR}/_common.sh"

    if [[ ! -f "${NETWORK_ENV}" ]]; then
      echo "ERROR: network.env not found. Run 02_create_l1.sh first." >&2
      exit 1
    fi

    # shellcheck source=/dev/null
    source "${NETWORK_ENV}"

    if [[ -z "${CHAIN_ID:-}" ]]; then
      echo "ERROR: CHAIN_ID not found in network.env" >&2
      exit 1
    fi

    # Prefer the dedicated Archive-RPC node (port 9656, --http-allowed-hosts=*,
    # no pruning) over the bombard RPC (port 9654) so Blockscout's container
    # Host header is accepted and historical state queries succeed. Fall back
    # to the bombard RPC for older deployments that haven't re-run
    # 03_deploy_l1_config.sh; that path will fail with "invalid host specified"
    # until the operator re-runs the deploy.
    if [[ -n "${BLOCKSCOUT_RPC_URL:-}" ]]; then
      RPC_URL="${BLOCKSCOUT_RPC_URL}"
    elif [[ -n "${ARCHIVE_RPC_URL:-}" ]]; then
      RPC_URL="${ARCHIVE_RPC_URL}"
    else
      RPC_URL="http://${BOOTSTRAP_IP}:9654/ext/bc/${CHAIN_ID}/rpc"
      echo "warning: ARCHIVE_RPC_URL not set in network.env." >&2
      echo "         re-run ./03_deploy_l1_config.sh to spin up the Archive-RPC node," >&2
      echo "         otherwise Blockscout will hit 'invalid host specified'." >&2
    fi

    exec "${ROOT_DIR}/scripts/blockscout-up.sh" \
      --rpc "${RPC_URL}" \
      --chain-name "Avalanche-Remote-Benchmark" \
      --chain-short-name "AVAX-REMOTE" \
      "$@"
    ;;
  smoke)
    exec "${ROOT_DIR}/scripts/blockscout-smoke.sh" "$@"
    ;;
  down)
    exec "${ROOT_DIR}/scripts/blockscout-down.sh" "$@"
    ;;
  *)
    echo "usage: ./blockscout.sh [up|smoke|down]" >&2
    exit 1
    ;;
esac
