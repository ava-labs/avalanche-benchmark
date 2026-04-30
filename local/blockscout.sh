#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

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
