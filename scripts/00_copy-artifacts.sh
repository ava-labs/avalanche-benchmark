#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"

remote_dir="/data/avalanche-benchmark"
env_file="$repo_root/.env"
plugin_id="srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"

source "$script_dir/lib.sh"

required_files=(
  "create-l1"
  "bombard"
  "avalanchego"
  "$plugin_id"
  ".env"
  "config/genesis.json"
  "config/chain-config.json"
  "config/node-config.json"
  "staking/node-ids.env"
  "staking/pchain/1/signer.key"
  "staking/pchain/1/staker.crt"
  "staking/pchain/1/staker.key"
  "staking/pchain/2/signer.key"
  "staking/pchain/2/staker.crt"
  "staking/pchain/2/staker.key"
)

for i in $(seq 1 15); do
  required_files+=(
    "staking/l1/$i/signer.key"
    "staking/l1/$i/staker.crt"
    "staking/l1/$i/staker.key"
  )
done

require_artifacts() {
  local missing=0
  local artifact
  for artifact in "${required_files[@]}"; do
    if [[ ! -f "$repo_root/$artifact" ]]; then
      echo "ERROR: missing $repo_root/$artifact" >&2
      missing=1
    fi
  done
  if [[ "$missing" -ne 0 ]]; then
    echo "Run 'make' first, and create .env from .env.example." >&2
    exit 1
  fi
}

collect_node_hosts() {
  local host
  declare -A seen=()
  node_hosts=()
  while IFS= read -r host; do
    if [[ -z "${seen[$host]:-}" ]]; then
      seen[$host]=1
      node_hosts+=("$host")
    fi
  done < <({ csv_env_values DC1_NODE_IPS; csv_env_values DC2_NODE_IPS; })
}

require_env_file
require_artifacts

benchmark_host_ip="$(require_env BENCHMARK_HOST_IP)"
collect_node_hosts

benchmark_work_dir="$(host_work_dir "$benchmark_host_ip")"
run_host_command "$benchmark_host_ip" 20s "pkill -f '[a]valanchego' || true"
copy_paths_to_host_dir "$benchmark_host_ip" "$benchmark_work_dir/bin" \
  "$repo_root/avalanchego" \
  "$repo_root/bombard"
copy_paths_to_host_dir "$benchmark_host_ip" "$benchmark_work_dir/plugins" "$repo_root/$plugin_id"
run_host_command "$benchmark_host_ip" 20s "chmod +x '$benchmark_work_dir/bin/avalanchego' '$benchmark_work_dir/bin/bombard' '$benchmark_work_dir/plugins/$plugin_id'"
echo "Copied benchmark assets to $benchmark_host_ip:$benchmark_work_dir"

for node_host in "${node_hosts[@]}"; do
  if [[ "$node_host" == "$benchmark_host_ip" ]] && ! is_local_host "$node_host"; then
    echo "ERROR: BENCHMARK_HOST_IP must not also appear in DC node IPs: $node_host" >&2
    exit 1
  fi

  node_work_dir="$(host_work_dir "$node_host")"
  run_host_command "$node_host" 20s "pkill -f '[a]valanchego' || true"
  copy_paths_to_host_dir "$node_host" "$node_work_dir/bin" "$repo_root/avalanchego"
  copy_paths_to_host_dir "$node_host" "$node_work_dir/plugins" "$repo_root/$plugin_id"
  run_host_command "$node_host" 20s "chmod +x '$node_work_dir/bin/avalanchego' '$node_work_dir/plugins/$plugin_id'"
  echo "Copied node assets to $node_host:$node_work_dir"
done
