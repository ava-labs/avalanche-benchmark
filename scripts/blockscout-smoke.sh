#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ROOT_DIR}/tmp/blockscout/blockscout.env"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "missing Blockscout env file: ${ENV_FILE}" >&2
  echo "run ./scripts/blockscout-up.sh first" >&2
  exit 1
fi

# shellcheck source=/dev/null
source "${ENV_FILE}"

echo "Blockscout API stats:"
curl -s "${BLOCKSCOUT_API_URL}/api/v2/stats"
echo
echo

echo "Blockscout frontend title:"
curl -s "${BLOCKSCOUT_FRONTEND_URL}" | tr -d '\n' | sed -n 's:.*<title>\(.*\)</title>.*:\1:p'
echo
