#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"

remote_dir="/data/avalanche-benchmark"
env_file="$repo_root/.env"
l1_env_file="$repo_root/runtime-data/l1.env"
node_ids_file="$repo_root/staking/node-ids.env"
plugin_id="srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"
readiness_timeout=120

source "$script_dir/lib.sh"

require_file() {
  local path="$1"
  if [[ ! -f "$path" ]]; then
    echo "ERROR: missing $path" >&2
    exit 1
  fi
}

collect_node_hosts() {
  local host
  declare -A seen=()
  node_hosts=()

  while IFS= read -r host; do
    if [[ -n "${seen[$host]:-}" ]]; then
      echo "ERROR: duplicate node host in inventory: $host" >&2
      exit 1
    fi
    seen[$host]=1
    node_hosts+=("$host")
  done < <({ csv_env_values DC1_NODE_IPS; csv_env_values DC2_NODE_IPS; })

  if [[ "${#node_hosts[@]}" -eq 0 ]]; then
    echo "ERROR: .env must set at least one node host in DC1_NODE_IPS or DC2_NODE_IPS" >&2
    exit 1
  fi
}

join_by_comma() {
  local IFS=,
  printf '%s' "$*"
}

bootstrap_ips_for_node() {
  local current_index="$1"
  local ips=("$benchmark_host_ip:9651" "$benchmark_host_ip:9653")

  for j in "${!node_hosts[@]}"; do
    if [[ "$j" == "$current_index" ]]; then
      continue
    fi
    ips+=("${node_hosts[$j]}:9651")
  done

  join_by_comma "${ips[@]}"
}

bootstrap_ids_for_node() {
  local current_index="$1"
  local ids=("$pchain_1_node_id" "$pchain_2_node_id")

  for j in "${!l1_node_ids[@]}"; do
    if [[ "$j" == "$current_index" ]]; then
      continue
    fi
    ids+=("${l1_node_ids[$j]}")
  done

  join_by_comma "${ids[@]}"
}

get_node_id() {
  local rpc="$1"
  local body
  body="$(curl -fsS --max-time 5 \
    -H 'Content-Type: application/json' \
    --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID","params":{}}' \
    "$rpc/ext/info" 2>/dev/null || true)"
  printf '%s' "$body" | sed -n 's/.*"nodeID"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}

chain_rpc_ready() {
  local rpc="$1"
  local body
  body="$(curl -fsS --max-time 5 \
    -H 'Content-Type: application/json' \
    --data '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}' \
    "$rpc" 2>/dev/null || true)"
  printf '%s' "$body" | grep -q '"result"'
}

wait_l1_ready() {
  local host="$1"
  local index="$2"
  local expected_node_id="$3"
  local info_rpc="http://$host:9650"
  local chain_rpc="$info_rpc/ext/bc/$l1_chain_id/rpc"
  local deadline=$((SECONDS + readiness_timeout))
  local actual_node_id

  while (( SECONDS < deadline )); do
    actual_node_id="$(get_node_id "$info_rpc")"
    if [[ "$actual_node_id" == "$expected_node_id" ]] && chain_rpc_ready "$chain_rpc"; then
      return
    fi
    sleep 1
  done

  if [[ -z "${actual_node_id:-}" ]]; then
    actual_node_id="<empty>"
  fi
  echo "ERROR: l1-$index on $host not ready after ${readiness_timeout}s: nodeID=$actual_node_id expected=$expected_node_id chainRPC=$chain_rpc" >&2
  exit 1
}

require_env_file
require_file "$l1_env_file"
require_file "$node_ids_file"
require_file "$repo_root/config/chain-config.json"

benchmark_host_ip="$(require_env BENCHMARK_HOST_IP)"
pchain_1_node_id="$(require_env_from_file "$node_ids_file" PCHAIN_1_NODE_ID)"
pchain_2_node_id="$(require_env_from_file "$node_ids_file" PCHAIN_2_NODE_ID)"
l1_subnet_id="$(require_env_from_file "$l1_env_file" L1_SUBNET_ID)"
l1_chain_id="$(require_env_from_file "$l1_env_file" L1_CHAIN_ID)"

collect_node_hosts

for node_host in "${node_hosts[@]}"; do
  if [[ "$node_host" == "$benchmark_host_ip" ]]; then
    echo "ERROR: BENCHMARK_HOST_IP must not appear in DC node IPs: $node_host" >&2
    exit 1
  fi
done

l1_node_ids=()
for i in "${!node_hosts[@]}"; do
  node_index=$((i + 1))
  for file in signer.key staker.crt staker.key; do
    require_file "$repo_root/staking/l1/$node_index/$file"
  done
  l1_node_ids+=("$(require_env_from_file "$node_ids_file" "L1_${node_index}_NODE_ID")")
done

for i in "${!node_hosts[@]}"; do
  node_index=$((i + 1))
  node_host="${node_hosts[$i]}"
  node_work_dir="$(host_work_dir "$node_host")"
  node_data_dir="$node_work_dir/runtime-data/l1"
  bootstrap_ips="$(bootstrap_ips_for_node "$i")"
  bootstrap_ids="$(bootstrap_ids_for_node "$i")"

  echo "Preparing l1-$node_index on $node_host"
  run_host_script "$node_host" 45s <<SCRIPT
set -euo pipefail
cd '$node_work_dir'
pkill -f '[a]valanchego' || true
rm -rf runtime-data/l1
mkdir -p \
  runtime-data/l1/staking \
  runtime-data/l1/plugins \
  runtime-data/l1/configs/chains/$l1_chain_id
test -x bin/avalanchego
test -x plugins/$plugin_id
cp -f plugins/$plugin_id runtime-data/l1/plugins/$plugin_id
chmod +x runtime-data/l1/plugins/$plugin_id
SCRIPT

  copy_paths_to_host_dir "$node_host" "$node_data_dir/staking" \
    "$repo_root/staking/l1/$node_index/signer.key" \
    "$repo_root/staking/l1/$node_index/staker.crt" \
    "$repo_root/staking/l1/$node_index/staker.key"

  copy_paths_to_host_dir "$node_host" "$node_data_dir/configs/chains/$l1_chain_id" \
    "$repo_root/config/chain-config.json"
  run_host_command "$node_host" 20s "mv -f '$node_data_dir/configs/chains/$l1_chain_id/chain-config.json' '$node_data_dir/configs/chains/$l1_chain_id/config.json'"

  run_host_script "$node_host" 30s <<SCRIPT
set -euo pipefail
cd '$node_work_dir'
nohup bin/avalanchego \
  --data-dir=runtime-data/l1 \
  --network-id=local \
  --sybil-protection-enabled=false \
  --http-host=0.0.0.0 \
  --public-ip=$node_host \
  --track-subnets=$l1_subnet_id \
  --bootstrap-ips=$bootstrap_ips \
  --bootstrap-ids=$bootstrap_ids \
  > runtime-data/l1/stdout.log 2>&1 &
disown || true
echo "started l1-$node_index pid=\$! rpc=http://$node_host:9650 log=$node_data_dir/stdout.log"
SCRIPT
done

for i in "${!node_hosts[@]}"; do
  node_index=$((i + 1))
  node_host="${node_hosts[$i]}"
  expected_node_id="$(require_env_from_file "$node_ids_file" "L1_${node_index}_NODE_ID")"
  wait_l1_ready "$node_host" "$node_index" "$expected_node_id"
done

echo "L1 nodes ready"
for i in "${!node_hosts[@]}"; do
  node_index=$((i + 1))
  node_host="${node_hosts[$i]}"
  node_work_dir="$(host_work_dir "$node_host")"
  expected_node_id="$(require_env_from_file "$node_ids_file" "L1_${node_index}_NODE_ID")"
  echo "  l1-$node_index RPC: http://$node_host:9650/ext/bc/$l1_chain_id/rpc expectedNodeID=$expected_node_id log=$node_work_dir/runtime-data/l1/stdout.log"
done
