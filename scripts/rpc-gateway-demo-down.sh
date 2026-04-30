#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="${ROOT_DIR}/tmp/rpc-gateway-demo"
ENV_FILE="${STATE_DIR}/demo.env"

if [[ -f "${ENV_FILE}" ]]; then
  # shellcheck source=/dev/null
  source "${ENV_FILE}"
fi

if [[ -f "${STATE_DIR}/gateway.pid" ]]; then
  GATEWAY_PID="$(cat "${STATE_DIR}/gateway.pid")"
  kill "${GATEWAY_PID}" 2>/dev/null || true
  rm -f "${STATE_DIR}/gateway.pid"
fi

pkill -f "${ROOT_DIR}/single-node/network_data" 2>/dev/null || true

if [[ -d "${STATE_DIR}/pgdata" ]]; then
  pg_ctl -D "${STATE_DIR}/pgdata" stop >/dev/null 2>&1 || true
fi

rm -rf "${STATE_DIR}"

echo "RPC gateway demo stopped."
