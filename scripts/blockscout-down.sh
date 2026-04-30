#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="${ROOT_DIR}/tmp/blockscout"
ENV_FILE="${STATE_DIR}/blockscout.env"
COMPOSE_FILE="${ROOT_DIR}/blockscout/docker-compose.yml"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "Blockscout is not running."
  exit 0
fi

# shellcheck source=/dev/null
source "${ENV_FILE}"

docker compose \
  --env-file "${ENV_FILE}" \
  -f "${COMPOSE_FILE}" \
  -p "${BLOCKSCOUT_PROJECT_NAME}" \
  down -v --remove-orphans >/dev/null 2>&1 || true

if [[ "${BLOCKSCOUT_CHAIN_STARTED_BY_SCRIPT:-0}" == "1" ]]; then
  pkill -f "${ROOT_DIR}/local/network_data" 2>/dev/null || true
fi

rm -rf "${STATE_DIR}"

echo "Blockscout stopped."
