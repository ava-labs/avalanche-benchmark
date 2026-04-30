#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="${ROOT_DIR}/tmp/rpc-gateway-demo"
ENV_FILE="${STATE_DIR}/demo.env"
CHAIN_LOG="${STATE_DIR}/chain.log"
GATEWAY_LOG="${STATE_DIR}/gateway.log"
PG_LOG="${STATE_DIR}/postgres.log"
PG_DATA_DIR="${STATE_DIR}/pgdata"

CHAIN_DIR="${ROOT_DIR}/single-node"
GATEWAY_DIR="${ROOT_DIR}/rpc-gateway"

GATEWAY_PORT="${RPC_GATEWAY_PORT:-18080}"
PG_PORT="${RPC_GATEWAY_PGPORT:-55432}"
PG_DB="${RPC_GATEWAY_PGDB:-rpc_gateway}"
PG_USER="${USER}"

DATABASE_URL="postgres://${PG_USER}@localhost:${PG_PORT}/${PG_DB}?sslmode=disable"
GATEWAY_RPC_URL="http://127.0.0.1:${GATEWAY_PORT}/rpc"

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

port_in_use() {
  local port="$1"
  lsof -ti "tcp:${port}" >/dev/null 2>&1
}

wait_for_http() {
  local url="$1"
  local attempts="${2:-60}"
  local i
  for ((i=1; i<=attempts; i++)); do
    if curl -sf "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_for_rpc() {
  local url="$1"
  local attempts="${2:-60}"
  local payload='{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}'
  local i
  for ((i=1; i<=attempts; i++)); do
    if curl -sf -H 'Content-Type: application/json' --data "${payload}" "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

need_cmd go
need_cmd curl
need_cmd initdb
need_cmd pg_ctl
need_cmd createdb
need_cmd psql
need_cmd lsof

mkdir -p "${STATE_DIR}"

if port_in_use "${GATEWAY_PORT}"; then
  echo "port ${GATEWAY_PORT} is already in use; set RPC_GATEWAY_PORT or free the port" >&2
  exit 1
fi

if port_in_use "${PG_PORT}"; then
  echo "port ${PG_PORT} is already in use; set RPC_GATEWAY_PGPORT or free the port" >&2
  exit 1
fi

echo "Stopping any previous single-node demo processes..."
pkill -f "${ROOT_DIR}/single-node/network_data" 2>/dev/null || true
sleep 1

echo "Resetting local Postgres state..."
rm -rf "${PG_DATA_DIR}" "${PG_LOG}"
initdb -D "${PG_DATA_DIR}" >/dev/null
pg_ctl -D "${PG_DATA_DIR}" -l "${PG_LOG}" -o "-p ${PG_PORT}" start >/dev/null
createdb -p "${PG_PORT}" "${PG_DB}"

echo "Starting local benchmark chain..."
(
  cd "${CHAIN_DIR}"
  BENCHMARK_DISK_REQUIRED_PERCENT="${BENCHMARK_DISK_REQUIRED_PERCENT:-1}" \
  BENCHMARK_DISK_WARNING_PERCENT="${BENCHMARK_DISK_WARNING_PERCENT:-2}" \
  go run ./cmd/benchmark --exit-on-success
) >"${CHAIN_LOG}" 2>&1

RAW_RPC_URL="$(cut -d, -f1 "${CHAIN_DIR}/network_data/rpcs.txt")"
if [[ -z "${RAW_RPC_URL}" ]]; then
  echo "failed to resolve upstream RPC URL from single-node/network_data/rpcs.txt" >&2
  exit 1
fi
if ! wait_for_rpc "${RAW_RPC_URL}" 30; then
  echo "upstream RPC did not become ready: ${RAW_RPC_URL}" >&2
  exit 1
fi

echo "Migrating and seeding gateway database..."
(
  cd "${GATEWAY_DIR}"
  DATABASE_URL="${DATABASE_URL}" make migrate >/dev/null
)

KEY_OUTPUT="$(cd "${GATEWAY_DIR}" && go run ./cmd/rpc-gateway keygen)"
RAW_KEY="$(printf '%s\n' "${KEY_OUTPUT}" | awk -F= '/^raw_key=/{print $2}')"
if [[ -z "${RAW_KEY}" ]]; then
  echo "failed to generate API key" >&2
  exit 1
fi

(
  cd "${GATEWAY_DIR}"
  DATABASE_URL="${DATABASE_URL}" RAW_KEY="${RAW_KEY}" make seed-bombard >/dev/null
)

echo "Starting RPC gateway..."
(
  cd "${GATEWAY_DIR}"
  nohup env \
    LISTEN_ADDR=":${GATEWAY_PORT}" \
    BENCHMARK_DATA_DIR="../single-node/network_data" \
    DATABASE_URL="${DATABASE_URL}" \
    go run ./cmd/rpc-gateway \
    >"${GATEWAY_LOG}" 2>&1 < /dev/null &
  echo $! >"${STATE_DIR}/gateway.pid"
)
GATEWAY_PID="$(cat "${STATE_DIR}/gateway.pid")"

if ! wait_for_http "http://127.0.0.1:${GATEWAY_PORT}/healthz" 30; then
  echo "gateway did not become healthy; see ${GATEWAY_LOG}" >&2
  exit 1
fi

cat >"${ENV_FILE}" <<EOF
export RAW_KEY='${RAW_KEY}'
export DATABASE_URL='${DATABASE_URL}'
export RAW_RPC_URL='${RAW_RPC_URL}'
export GATEWAY_RPC_URL='${GATEWAY_RPC_URL}'
export RPC_GATEWAY_PORT='${GATEWAY_PORT}'
export RPC_GATEWAY_PGPORT='${PG_PORT}'
export RPC_GATEWAY_STATE_DIR='${STATE_DIR}'
export RPC_GATEWAY_CHAIN_LOG='${CHAIN_LOG}'
export RPC_GATEWAY_LOG='${GATEWAY_LOG}'
EOF

echo
echo "RPC gateway demo is ready."
echo "source ${ENV_FILE}"
echo "./scripts/rpc-gateway-demo-smoke.sh"
echo "./scripts/rpc-gateway-demo-bombard.sh -erc20 -tps 250"
echo "./scripts/rpc-gateway-demo-down.sh"
