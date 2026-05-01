#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
STATE_DIR="${ROOT_DIR}/tmp/blockscout"
ENV_FILE="${STATE_DIR}/blockscout.env"
CHAIN_LOG="${STATE_DIR}/chain.log"
COMPOSE_FILE="${ROOT_DIR}/blockscout/docker-compose.yml"
IMAGES_BUNDLE="${BLOCKSCOUT_IMAGES_BUNDLE:-${ROOT_DIR}/blockscout/images.tar.gz}"

# Source tree: startnetwork and network_data live under local/.
# Offline pack (flat extraction): they live at the root alongside this script's
# parent directory.
if [[ -e "${ROOT_DIR}/bin/startnetwork" || -f "${ROOT_DIR}/network_data/rpcs.txt" ]]; then
  LOCAL_DIR="${ROOT_DIR}"
else
  LOCAL_DIR="${ROOT_DIR}/local"
fi

PROJECT_NAME="${BLOCKSCOUT_PROJECT_NAME:-avalanche-benchmark-blockscout}"
API_PORT="${BLOCKSCOUT_API_PORT:-4000}"
FRONTEND_PORT="${BLOCKSCOUT_FRONTEND_PORT:-4001}"

RPC_URL="${BLOCKSCOUT_RPC_URL:-}"
CHAIN_NAME="${BLOCKSCOUT_CHAIN_NAME:-Avalanche-Benchmark}"
CHAIN_SHORT_NAME="${BLOCKSCOUT_CHAIN_SHORT_NAME:-AVAX-BENCH}"
START_LOCAL=0

# shellcheck source=_blockscout_runtime.sh
source "${SCRIPT_DIR}/_blockscout_runtime.sh"
# shellcheck source=/dev/null
source "${ROOT_DIR}/blockscout/images.conf"

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

port_in_use() {
  local port="$1"
  if command -v lsof >/dev/null 2>&1; then
    lsof -ti "tcp:${port}" >/dev/null 2>&1
  elif command -v ss >/dev/null 2>&1; then
    ss -tln 2>/dev/null | awk '{print $4}' | grep -Eq "[:.]${port}$"
  elif command -v netstat >/dev/null 2>&1; then
    netstat -tln 2>/dev/null | awk '{print $4}' | grep -Eq "[:.]${port}$"
  else
    return 1
  fi
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

# Resolve host.docker.internal to an actual IP from a container's perspective.
# Elixir's HTTP client (used by Blockscout) bypasses /etc/hosts and goes
# straight to DNS, so the magic hostname doesn't resolve. We run a throwaway
# container with the same extra_hosts mapping and read the IP it sees.
resolve_host_gateway() {
  local img="${BLOCKSCOUT_REDIS_IMAGE:-docker.io/library/redis:7-alpine}"
  "${RUNTIME}" run --rm \
    --add-host=host.docker.internal:host-gateway \
    "${img}" \
    sh -c 'awk "/host.docker.internal/ {print \$1; exit}" /etc/hosts' 2>/dev/null
}

# True if $1 is an IP bound to one of this host's interfaces. Used to rewrite
# remote-style URLs that happen to point at the operator host itself (which
# the container cannot reach via its public IP under rootless podman).
is_local_ip() {
  local ip="$1"
  if command -v ip >/dev/null 2>&1; then
    ip -4 -o addr show 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | grep -qx "${ip}"
  elif command -v ifconfig >/dev/null 2>&1; then
    ifconfig 2>/dev/null | awk '/inet / {print $2}' | grep -qx "${ip}"
  else
    return 1
  fi
}

to_internal_url() {
  local url="$1"
  local ip="${BLOCKSCOUT_HOST_GATEWAY_IP:-}"
  if [[ -z "${ip}" ]]; then
    ip="$(resolve_host_gateway)"
  fi
  if [[ -z "${ip}" ]]; then
    # Fall back to the magic hostname; works for runtimes whose HTTP client
    # consults /etc/hosts (e.g. Docker Desktop on Mac).
    ip="host.docker.internal"
  fi
  url="${url/http:\/\/127.0.0.1/http:\/\/${ip}}"
  url="${url/http:\/\/localhost/http:\/\/${ip}}"
  url="${url/ws:\/\/127.0.0.1/ws:\/\/${ip}}"
  url="${url/ws:\/\/localhost/ws:\/\/${ip}}"
  url="${url/https:\/\/127.0.0.1/https:\/\/${ip}}"
  url="${url/https:\/\/localhost/https:\/\/${ip}}"
  url="${url/wss:\/\/127.0.0.1/wss:\/\/${ip}}"
  url="${url/wss:\/\/localhost/wss:\/\/${ip}}"

  # Also rewrite when the URL points at one of the operator host's own
  # interfaces — under rootless podman the container can't always reach the
  # host's eth0 IP directly, but it can always reach host-gateway.
  local re='^([a-z]+)://([^:/]+)(:[0-9]+)?(/.*)?$'
  if [[ "${url}" =~ $re ]]; then
    local scheme="${BASH_REMATCH[1]}"
    local host="${BASH_REMATCH[2]}"
    local port="${BASH_REMATCH[3]}"
    local path="${BASH_REMATCH[4]}"
    if [[ "${host}" =~ ^[0-9.]+$ ]] && is_local_ip "${host}"; then
      url="${scheme}://${ip}${port}${path}"
    fi
  fi
  echo "${url}"
}

to_ws_url() {
  local url="$1"
  url="${url%/rpc}/ws"
  url="${url/http:\/\//ws:\/\/}"
  url="${url/https:\/\//wss:\/\/}"
  echo "${url}"
}

ensure_images_loaded() {
  local missing=()
  for img in "${BLOCKSCOUT_BACKEND_IMAGE}" "${BLOCKSCOUT_FRONTEND_IMAGE}" \
             "${BLOCKSCOUT_POSTGRES_IMAGE}" "${BLOCKSCOUT_REDIS_IMAGE}"; do
    if ! "${RUNTIME}" image inspect "${img}" >/dev/null 2>&1; then
      missing+=("${img}")
    fi
  done

  if [[ ${#missing[@]} -eq 0 ]]; then
    return 0
  fi

  if [[ -f "${IMAGES_BUNDLE}" ]]; then
    echo "Loading Blockscout images from ${IMAGES_BUNDLE}..."
    "${RUNTIME}" load -i "${IMAGES_BUNDLE}"
    missing=()
    for img in "${BLOCKSCOUT_BACKEND_IMAGE}" "${BLOCKSCOUT_FRONTEND_IMAGE}" \
               "${BLOCKSCOUT_POSTGRES_IMAGE}" "${BLOCKSCOUT_REDIS_IMAGE}"; do
      if ! "${RUNTIME}" image inspect "${img}" >/dev/null 2>&1; then
        missing+=("${img}")
      fi
    done
  fi

  if [[ ${#missing[@]} -gt 0 ]]; then
    echo "missing images and no usable bundle at ${IMAGES_BUNDLE}:" >&2
    printf '  %s\n' "${missing[@]}" >&2
    echo "build the bundle on a machine with internet:" >&2
    echo "  ./scripts/blockscout-pack-images.sh" >&2
    exit 1
  fi
}

usage() {
  cat <<EOF
Usage: ./scripts/blockscout-up.sh [--rpc URL] [--chain-name NAME] [--chain-short-name NAME] [--start-local]

If --rpc is omitted, the script will try to reuse a running local benchmark chain
from local/network_data/rpcs.txt.

Use --start-local if you want this script to start the local benchmark chain for you
when no running local RPC is found.

Environment overrides:
  BLOCKSCOUT_RUNTIME=podman|docker     pin a specific container runtime
  BLOCKSCOUT_IMAGES_BUNDLE=PATH        override the offline image archive
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

detect_runtime
ensure_images_loaded

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

# Prefer the dedicated Archive-RPC node (--http-host=0.0.0.0,
# --http-allowed-hosts=*, no pruning) so a podman container reaching via
# host-gateway can talk to it. Fall back to rpcs.txt only if the user hasn't
# enabled an Archive-RPC node — that path will fail with "invalid host
# specified" unless avalanchego's allowed hosts have been manually loosened.
if [[ -z "${RPC_URL}" ]]; then
  if [[ -f "${LOCAL_DIR}/network_data/archive-rpcs.txt" ]]; then
    RPC_URL="$(awk -F, '{print $NF}' "${LOCAL_DIR}/network_data/archive-rpcs.txt")"
  elif [[ -f "${LOCAL_DIR}/network_data/rpcs.txt" ]]; then
    RPC_URL="$(awk -F, '{print $NF}' "${LOCAL_DIR}/network_data/rpcs.txt")"
    echo "warning: no archive-rpcs.txt found; using bombard RPC at ${RPC_URL}." >&2
    echo "         Blockscout will likely hit 'invalid host specified' from the container." >&2
    echo "         Set l1ArchiveRpcs >= 1 in benchmark-config.json and restart the network." >&2
  fi
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
BLOCKSCOUT_BACKEND_IMAGE=${BLOCKSCOUT_BACKEND_IMAGE}
BLOCKSCOUT_FRONTEND_IMAGE=${BLOCKSCOUT_FRONTEND_IMAGE}
BLOCKSCOUT_POSTGRES_IMAGE=${BLOCKSCOUT_POSTGRES_IMAGE}
BLOCKSCOUT_REDIS_IMAGE=${BLOCKSCOUT_REDIS_IMAGE}
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
BLOCKSCOUT_RUNTIME=${RUNTIME}
EOF

echo "Starting Blockscout (${RUNTIME})..."
"${COMPOSE[@]}" \
  --env-file "${ENV_FILE}" \
  -f "${COMPOSE_FILE}" \
  -p "${PROJECT_NAME}" \
  up -d

if ! wait_for_http "http://127.0.0.1:${API_PORT}/api/v2/stats" 90; then
  echo "Blockscout backend did not become ready." >&2
  echo "Inspect with: ${COMPOSE[*]} --env-file ${ENV_FILE} -f ${COMPOSE_FILE} -p ${PROJECT_NAME} logs backend" >&2
  exit 1
fi

if ! wait_for_http "http://127.0.0.1:${FRONTEND_PORT}" 60; then
  echo "Blockscout frontend did not become ready." >&2
  echo "Inspect with: ${COMPOSE[*]} --env-file ${ENV_FILE} -f ${COMPOSE_FILE} -p ${PROJECT_NAME} logs frontend" >&2
  exit 1
fi

echo
echo "Blockscout is ready (${RUNTIME})."
echo "source ${ENV_FILE}"
echo "./scripts/blockscout-smoke.sh"
echo "./scripts/blockscout-down.sh"
echo
echo "Frontend: http://127.0.0.1:${FRONTEND_PORT}"
echo "API:      http://127.0.0.1:${API_PORT}/api/v2/stats"
