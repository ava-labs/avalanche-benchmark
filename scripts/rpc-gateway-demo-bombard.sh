#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ROOT_DIR}/tmp/rpc-gateway-demo/demo.env"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "missing demo env file: ${ENV_FILE}" >&2
  echo "run ./scripts/rpc-gateway-demo-up.sh first" >&2
  exit 1
fi

# shellcheck source=/dev/null
source "${ENV_FILE}"

if [[ $# -eq 0 ]]; then
  set -- -erc20 -tps 250
fi

cd "${ROOT_DIR}/single-node"
go run ./cmd/bombard \
  -rpc "${GATEWAY_RPC_URL}" \
  -api-key "${RAW_KEY}" \
  "$@"
