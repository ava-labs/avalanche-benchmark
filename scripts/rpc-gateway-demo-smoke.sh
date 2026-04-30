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

echo "Allowed method:"
curl -s "${GATEWAY_RPC_URL}" \
  -H 'Content-Type: application/json' \
  -H "X-API-Key: ${RAW_KEY}" \
  --data '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}'
echo
echo

echo "Denied method:"
curl -s "${GATEWAY_RPC_URL}" \
  -H 'Content-Type: application/json' \
  -H "X-API-Key: ${RAW_KEY}" \
  --data '{"jsonrpc":"2.0","id":2,"method":"eth_getLogs","params":[{}]}'
echo
echo

echo "Missing API key:"
curl -s "${GATEWAY_RPC_URL}" \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":3,"method":"eth_chainId","params":[]}'
echo
