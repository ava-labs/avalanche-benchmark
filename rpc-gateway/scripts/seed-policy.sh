#!/usr/bin/env bash

set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 <demo|bombard> <raw-api-key>" >&2
  exit 1
fi

MODE="$1"
RAW_KEY="$2"
DATABASE_URL="${DATABASE_URL:-postgres://rpc_gateway:rpc_gateway@localhost:5432/rpc_gateway}"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

case "$MODE" in
  demo)
    SQL_FILE="${ROOT_DIR}/examples/demo-seed.sql"
    ;;
  bombard)
    SQL_FILE="${ROOT_DIR}/examples/bombard-seed.sql"
    ;;
  *)
    echo "invalid mode: ${MODE}" >&2
    exit 1
    ;;
esac

HASH_LINE="$(cd "${ROOT_DIR}" && go run ./cmd/rpc-gateway hash-key "${RAW_KEY}")"
KEY_HASH="${HASH_LINE#sha256=}"

sed "s/REPLACE_WITH_SHA256_HASH/${KEY_HASH}/g" "${SQL_FILE}" | psql "${DATABASE_URL}" >/dev/null
echo "seeded ${MODE} policy with hash ${KEY_HASH}"
