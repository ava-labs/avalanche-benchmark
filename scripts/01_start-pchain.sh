#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"

remote_dir="/data/avalanche-benchmark"
env_file="$repo_root/.env"
node_ids_file="$repo_root/staking/node-ids.env"
plugin_id="srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"

source "$script_dir/lib.sh"

require_env_file

if [[ ! -f "$node_ids_file" ]]; then
  echo "ERROR: missing $node_ids_file" >&2
  exit 1
fi

benchmark_host_ip="$(require_env BENCHMARK_HOST_IP)"
pchain_1_node_id="$(require_env_from_file "$node_ids_file" PCHAIN_1_NODE_ID)"
pchain_2_node_id="$(require_env_from_file "$node_ids_file" PCHAIN_2_NODE_ID)"
benchmark_work_dir="$(host_work_dir "$benchmark_host_ip")"

wait_healthy() {
  local name="$1"
  local rpc="$2"
  local deadline=$((SECONDS + 300))
  local body

  while (( SECONDS < deadline )); do
    body="$(curl -fsS --max-time 2 "$rpc/ext/health" 2>/dev/null || true)"
    if printf '%s' "$body" | tr -d '[:space:]' | grep -q '"healthy":true'; then
      return
    fi
    sleep 1
  done

  echo "ERROR: $name did not become healthy at $rpc" >&2
  exit 1
}

get_node_id() {
  local rpc="$1"
  local body
  body="$(curl -fsS --max-time 5 \
    -H 'Content-Type: application/json' \
    --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID","params":{}}' \
    "$rpc/ext/info")"
  printf '%s' "$body" | sed -n 's/.*"nodeID"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}

verify_node_id() {
  local name="$1"
  local rpc="$2"
  local expected="$3"
  local actual
  actual="$(get_node_id "$rpc")"
  if [[ -z "$actual" ]]; then
    echo "ERROR: $name returned empty nodeID from $rpc" >&2
    exit 1
  fi
  if [[ "$actual" != "$expected" ]]; then
    echo "ERROR: $name nodeID mismatch: got $actual, want $expected" >&2
    exit 1
  fi
}

run_host_script "$benchmark_host_ip" 45s <<SCRIPT
set -euo pipefail
cd '$benchmark_work_dir'
pkill -f '[a]valanchego' || true
rm -rf runtime-data
mkdir -p \
  runtime-data/pchain-1/staking \
  runtime-data/pchain-1/plugins \
  runtime-data/pchain-2/staking \
  runtime-data/pchain-2/plugins
test -x plugins/$plugin_id
cp -f plugins/$plugin_id runtime-data/pchain-1/plugins/$plugin_id
cp -f plugins/$plugin_id runtime-data/pchain-2/plugins/$plugin_id
chmod +x runtime-data/pchain-1/plugins/$plugin_id runtime-data/pchain-2/plugins/$plugin_id
SCRIPT

copy_paths_to_host_dir "$benchmark_host_ip" "$benchmark_work_dir/runtime-data/pchain-1/staking" \
  "$repo_root/staking/pchain/1/signer.key" \
  "$repo_root/staking/pchain/1/staker.crt" \
  "$repo_root/staking/pchain/1/staker.key"
copy_paths_to_host_dir "$benchmark_host_ip" "$benchmark_work_dir/runtime-data/pchain-2/staking" \
  "$repo_root/staking/pchain/2/signer.key" \
  "$repo_root/staking/pchain/2/staker.crt" \
  "$repo_root/staking/pchain/2/staker.key"
copy_paths_to_host_dir "$benchmark_host_ip" "$benchmark_work_dir/runtime-data/pchain-1" "$repo_root/config/node-config.json"
copy_paths_to_host_dir "$benchmark_host_ip" "$benchmark_work_dir/runtime-data/pchain-2" "$repo_root/config/node-config.json"

run_host_script "$benchmark_host_ip" 30s <<SCRIPT
set -euo pipefail
cd '$benchmark_work_dir'
test -x bin/avalanchego

start_node() {
  local name="\$1"
  local http_port="\$2"
  local staking_port="\$3"
  local bootstrap_ip="\$4"
  local bootstrap_id="\$5"
  local data_dir="runtime-data/\$name"
  local log="\$data_dir/stdout.log"

  nohup bin/avalanchego \
    "--data-dir=\$data_dir" \
    "--config-file=\$data_dir/node-config.json" \
    --network-id=local \
    --sybil-protection-enabled=false \
    --http-host=0.0.0.0 \
    "--http-port=\$http_port" \
    "--staking-port=\$staking_port" \
    "--public-ip=$benchmark_host_ip" \
    "--bootstrap-ips=\$bootstrap_ip" \
    "--bootstrap-ids=\$bootstrap_id" \
    > "\$log" 2>&1 &

  echo "started \$name pid=\$! rpc=http://$benchmark_host_ip:\$http_port log=$benchmark_work_dir/\$log"
}

start_node pchain-1 9650 9651 "" ""
start_node pchain-2 9652 9653 "$benchmark_host_ip:9651" "$pchain_1_node_id"
SCRIPT

pchain_1_rpc="http://$benchmark_host_ip:9650"
pchain_2_rpc="http://$benchmark_host_ip:9652"

wait_healthy pchain-1 "$pchain_1_rpc"
wait_healthy pchain-2 "$pchain_2_rpc"
verify_node_id pchain-1 "$pchain_1_rpc" "$pchain_1_node_id"
verify_node_id pchain-2 "$pchain_2_rpc" "$pchain_2_node_id"

echo "P-Chain nodes ready"
echo "  pchain-1 RPC: $pchain_1_rpc expectedNodeID=$pchain_1_node_id log=$benchmark_work_dir/runtime-data/pchain-1/stdout.log"
echo "  pchain-2 RPC: $pchain_2_rpc expectedNodeID=$pchain_2_node_id log=$benchmark_work_dir/runtime-data/pchain-2/stdout.log"
