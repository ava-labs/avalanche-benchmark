#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# Source tree: scripts/ and blockscout/ live as siblings of local/.
# Offline pack: scripts/ and blockscout/ live alongside this wrapper.
if [[ -d "${SCRIPT_DIR}/scripts" && -f "${SCRIPT_DIR}/blockscout/docker-compose.yml" ]]; then
  ROOT_DIR="${SCRIPT_DIR}"
else
  ROOT_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
fi

if [[ ! -f "${ROOT_DIR}/scripts/blockscout-up.sh" ]]; then
  echo "Blockscout helpers not found under ${ROOT_DIR}." >&2
  echo "If you extracted local-benchmark.tar.gz, rebuild the bundle with the explorer:" >&2
  echo "  cd local && make pack-blockscout" >&2
  exit 1
fi

ACTION="up"
if [[ $# -gt 0 ]]; then
  case "$1" in
    up|smoke|down)
      ACTION="$1"
      shift
      ;;
  esac
fi

case "${ACTION}" in
  up)
    exec "${ROOT_DIR}/scripts/blockscout-up.sh" \
      --chain-name "Avalanche-Local-Benchmark" \
      --chain-short-name "AVAX-LOCAL" \
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
