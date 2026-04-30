#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

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

    RPC_URL="${BLOCKSCOUT_RPC_URL:-http://${BOOTSTRAP_IP}:9654/ext/bc/${CHAIN_ID}/rpc}"

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
