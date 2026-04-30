#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="${ROOT_DIR}/tmp/blockscout"
ENV_FILE="${STATE_DIR}/blockscout.env"
CHAIN_LOG="${STATE_DIR}/chain.log"
COMPOSE_FILE="${ROOT_DIR}/blockscout/docker-compose.yml"
LOCAL_DIR="${ROOT_DIR}/local"

PROJECT_NAME="${BLOCKSCOUT_PROJECT_NAME:-avalanche-benchmark-blockscout}"
API_PORT="${BLOCKSCOUT_API_PORT:-4000}"
FRONTEND_PORT="${BLOCKSCOUT_FRONTEND_PORT:-4001}"

RPC_URL="${BLOCKSCOUT_RPC_URL:-}"
CHAIN_NAME="${BLOCKSCOUT_CHAIN_NAME:-Avalanche-Benchmark}"
CHAIN_SHORT_NAME="${BLOCKSCOUT_CHAIN_SHORT_NAME:-AVAX-BENCH}"
START_LOCAL=0

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
  local attempts="${2:-90}"
  local i
  for ((i=1; i<=attempts; i++)); do
    if curl -sf "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
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

rpc_result() {
  local url="$1"
  local method="$2"
  local params="${3:-[]}"
  curl -sf -H 'Content-Type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"${method}\",\"params\":${params}}" \
    "${url}" | sed -n 's/.*"result":"\([^"]*\)".*/\1/p'
}

to_internal_url() {
  local url="$1"
  url="${url/http:\/\/127.0.0.1/http:\/\/host.docker.internal}"
  url="${url/http:\/\/localhost/http:\/\/host.docker.internal}"
  url="${url/ws:\/\/127.0.0.1/ws:\/\/host.docker.internal}"
  url="${url/ws:\/\/localhost/ws:\/\/host.docker.internal}"
  url="${url/https:\/\/127.0.0.1/https:\/\/host.docker.internal}"
  url="${url/https:\/\/localhost/https:\/\/host.docker.internal}"
  url="${url/wss:\/\/127.0.0.1/wss:\/\/host.docker.internal}"
  url="${url/wss:\/\/localhost/wss:\/\/host.docker.internal}"
  echo "${url}"
}

to_ws_url() {
  local url="$1"
  url="${url%/rpc}/ws"
  url="${url/http:\/\//ws:\/\/}"
  url="${url/https:\/\//wss:\/\/}"
  echo "${url}"
}

usage() {
  cat <<EOF
Usage: ./scripts/blockscout-up.sh [--rpc URL] [--chain-name NAME] [--chain-short-name NAME] [--start-local]

If --rpc is omitted, the script will try to reuse a running local benchmark chain
from local/network_data/rpcs.txt.

Use --start-local if you want this script to start the local benchmark chain for you
when no running local RPC is found.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --rpc)
      RPC_URL="${2:-}"
      shift 2
      ;;
    --chain-name)
      CHAIN_NAME="${2:-}"
      shift 2
      ;;
    --chain-short-name)
      CHAIN_SHORT_NAME="${2:-}"
      shift 2
      ;;
    --start-local)
      START_LOCAL=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

need_cmd curl
need_cmd docker
need_cmd lsof

docker compose version >/dev/null

mkdir -p "${STATE_DIR}"

if port_in_use "${API_PORT}"; then
  echo "port ${API_PORT} is already in use; set BLOCKSCOUT_API_PORT or free the port" >&2
  exit 1
fi

if port_in_use "${FRONTEND_PORT}"; then
  echo "port ${FRONTEND_PORT} is already in use; set BLOCKSCOUT_FRONTEND_PORT or free the port" >&2
  exit 1
fi

RPC_URL="${RPC_URL%%,*}"
CHAIN_STARTED_BY_SCRIPT=0

if [[ -z "${RPC_URL}" && -f "${LOCAL_DIR}/network_data/rpcs.txt" ]]; then
  RPC_URL="$(awk -F, '{print $NF}' "${LOCAL_DIR}/network_data/rpcs.txt")"
fi

if [[ -n "${RPC_URL}" ]] && wait_for_rpc "${RPC_URL}" 5; then
  if [[ "${RPC_URL}" == http://127.0.0.1:* || "${RPC_URL}" == http://localhost:* ]]; then
    echo "Reusing existing local benchmark chain at ${RPC_URL}"
  else
    echo "Using explicit benchmark RPC at ${RPC_URL}"
  fi
elif [[ -z "${BLOCKSCOUT_RPC_URL:-}" && "${START_LOCAL}" == "1" ]]; then
  echo "Starting local benchmark chain..."
  rm -f "${CHAIN_LOG}"
  (
    cd "${LOCAL_DIR}"
    need_cmd go
    export BENCHMARK_DISK_REQUIRED_PERCENT="${BENCHMARK_DISK_REQUIRED_PERCENT:-1}"
    export BENCHMARK_DISK_WARNING_PERCENT="${BENCHMARK_DISK_WARNING_PERCENT:-2}"
    if [[ -x ./bin/startnetwork ]]; then
      ./bin/startnetwork --exit-on-success
    else
      go run ./cmd/startnetwork --exit-on-success
    fi
  ) >"${CHAIN_LOG}" 2>&1
  CHAIN_STARTED_BY_SCRIPT=1

  RPC_URL="$(awk -F, '{print $NF}' "${LOCAL_DIR}/network_data/rpcs.txt")"
  if [[ -z "${RPC_URL}" ]]; then
    echo "failed to resolve RPC URL from local/network_data/rpcs.txt" >&2
    exit 1
  fi
  if ! wait_for_rpc "${RPC_URL}" 30; then
    echo "benchmark RPC did not become ready: ${RPC_URL}" >&2
    exit 1
  fi
else
  echo "no running benchmark RPC found." >&2
  echo "start the local network first with:" >&2
  echo "  cd local && ./bin/startnetwork --exit-on-success" >&2
  echo "or run this script with --rpc URL or --start-local" >&2
  exit 1
fi

CHAIN_ID="${RPC_URL#*/ext/bc/}"
CHAIN_ID="${CHAIN_ID%%/rpc*}"

WS_URL_PUBLIC="$(to_ws_url "${RPC_URL}")"
RPC_URL_INTERNAL="$(to_internal_url "${RPC_URL}")"
WS_URL_INTERNAL="$(to_internal_url "${WS_URL_PUBLIC}")"

EVM_CHAIN_ID_HEX="$(rpc_result "${RPC_URL}" "eth_chainId")"
if [[ -z "${EVM_CHAIN_ID_HEX}" ]]; then
  echo "failed to query eth_chainId from ${RPC_URL}" >&2
  exit 1
fi
EVM_CHAIN_ID="$((16#${EVM_CHAIN_ID_HEX#0x}))"

SECRET_KEY_BASE="$(openssl rand -hex 32 2>/dev/null || date +%s%N)"

cat >"${ENV_FILE}" <<EOF
BLOCKSCOUT_PROJECT_NAME=${PROJECT_NAME}
BLOCKSCOUT_API_PORT=${API_PORT}
BLOCKSCOUT_FRONTEND_PORT=${FRONTEND_PORT}
BLOCKSCOUT_FRONTEND_URL=http://127.0.0.1:${FRONTEND_PORT}
BLOCKSCOUT_API_URL=http://127.0.0.1:${API_PORT}
BLOCKSCOUT_BACKEND_IMAGE=${BLOCKSCOUT_BACKEND_IMAGE:-blockscout/blockscout:latest}
BLOCKSCOUT_FRONTEND_IMAGE=${BLOCKSCOUT_FRONTEND_IMAGE:-ghcr.io/blockscout/frontend:latest}
BLOCKSCOUT_POSTGRES_IMAGE=${BLOCKSCOUT_POSTGRES_IMAGE:-postgres:15-alpine}
BLOCKSCOUT_REDIS_IMAGE=${BLOCKSCOUT_REDIS_IMAGE:-redis:7-alpine}
BLOCKSCOUT_DB_NAME=blockscout
BLOCKSCOUT_DB_USER=blockscout
BLOCKSCOUT_DB_PASSWORD=blockscout
BLOCKSCOUT_SECRET_KEY_BASE=${SECRET_KEY_BASE}
BLOCKSCOUT_EXTRA_HOST=${BLOCKSCOUT_EXTRA_HOST:-host.docker.internal:host-gateway}
BLOCKSCOUT_CHAIN_NAME=${CHAIN_NAME}
BLOCKSCOUT_CHAIN_SHORT_NAME=${CHAIN_SHORT_NAME}
BLOCKSCOUT_CHAIN_ID=${CHAIN_ID}
BLOCKSCOUT_EVM_CHAIN_ID=${EVM_CHAIN_ID}
BLOCKSCOUT_COIN_NAME=${BLOCKSCOUT_COIN_NAME:-AVAX}
BLOCKSCOUT_COIN_SYMBOL=${BLOCKSCOUT_COIN_SYMBOL:-AVAX}
BLOCKSCOUT_RPC_URL_PUBLIC=${RPC_URL}
BLOCKSCOUT_WS_URL_PUBLIC=${WS_URL_PUBLIC}
BLOCKSCOUT_RPC_URL_INTERNAL=${RPC_URL_INTERNAL}
BLOCKSCOUT_WS_URL_INTERNAL=${WS_URL_INTERNAL}
BLOCKSCOUT_TRACE_URL_INTERNAL=${RPC_URL_INTERNAL}
BLOCKSCOUT_CHAIN_STARTED_BY_SCRIPT=${CHAIN_STARTED_BY_SCRIPT}
BLOCKSCOUT_CHAIN_LOG=${CHAIN_LOG}
EOF

echo "Starting Blockscout..."
docker compose \
  --env-file "${ENV_FILE}" \
  -f "${COMPOSE_FILE}" \
  -p "${PROJECT_NAME}" \
  up -d

if ! wait_for_http "http://127.0.0.1:${API_PORT}/api/v2/stats" 90; then
  echo "Blockscout backend did not become ready." >&2
  echo "Inspect with: docker compose --env-file ${ENV_FILE} -f ${COMPOSE_FILE} -p ${PROJECT_NAME} logs backend" >&2
  exit 1
fi

if ! wait_for_http "http://127.0.0.1:${FRONTEND_PORT}" 60; then
  echo "Blockscout frontend did not become ready." >&2
  echo "Inspect with: docker compose --env-file ${ENV_FILE} -f ${COMPOSE_FILE} -p ${PROJECT_NAME} logs frontend" >&2
  exit 1
fi

echo
echo "Blockscout is ready."
echo "source ${ENV_FILE}"
echo "./scripts/blockscout-smoke.sh"
echo "./scripts/blockscout-down.sh"
echo
echo "Frontend: http://127.0.0.1:${FRONTEND_PORT}"
echo "API:      http://127.0.0.1:${API_PORT}/api/v2/stats"
