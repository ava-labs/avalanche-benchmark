#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
STATE_DIR="${ROOT_DIR}/tmp/blockscout"
ENV_FILE="${STATE_DIR}/blockscout.env"
COMPOSE_FILE="${ROOT_DIR}/blockscout/docker-compose.yml"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "Blockscout is not running."
  exit 0
fi

# shellcheck source=/dev/null
source "${ENV_FILE}"

# shellcheck source=_blockscout_runtime.sh
source "${SCRIPT_DIR}/_blockscout_runtime.sh"

# Reuse the runtime that brought it up, if recorded.
if [[ -n "${BLOCKSCOUT_RUNTIME:-}" ]]; then
  export BLOCKSCOUT_RUNTIME
fi

detect_runtime

"${COMPOSE[@]}" \
  --env-file "${ENV_FILE}" \
  -f "${COMPOSE_FILE}" \
  -p "${BLOCKSCOUT_PROJECT_NAME}" \
  down -v --remove-orphans >/dev/null 2>&1 || true

if [[ "${BLOCKSCOUT_CHAIN_STARTED_BY_SCRIPT:-0}" == "1" ]]; then
  pkill -f "${ROOT_DIR}/local/network_data" 2>/dev/null || true
fi

rm -rf "${STATE_DIR}"

echo "Blockscout stopped."
