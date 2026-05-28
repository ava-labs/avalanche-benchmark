#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/../.." && pwd)"

source "$repo_root/scripts/lib.sh"

usage() {
  echo "usage: $0 <node-number>" >&2
  exit 2
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

require_node_number() {
  if [[ "$#" -ne 1 ]]; then
    usage
  fi
  if [[ ! "$1" =~ ^[0-9]+$ || "$1" -lt 1 || "$1" -gt "${#node_hosts[@]}" ]]; then
    usage
  fi
  node_number="$1"
}

require_env_file
collect_node_hosts
require_node_number "$@"

benchmark_host_ip="$(require_env BENCHMARK_HOST_IP)"
node_host="${node_hosts[$((node_number - 1))]}"
node_work_dir="$(host_work_dir "$node_host")"

if [[ "$node_host" == "$benchmark_host_ip" ]]; then
  echo "ERROR: refusing to operate on benchmark host listed as node $node_number: $node_host" >&2
  exit 1
fi

echo "Stopping and wiping l1-$node_number on $node_host"
run_host_script "$node_host" 30s <<SCRIPT
set -euo pipefail
cd '$node_work_dir'
pkill -TERM -f 'runtime-data/l1' || true
sleep 2
pkill -KILL -f 'runtime-data/l1' || true
rm -rf runtime-data/l1
SCRIPT
echo "l1-$node_number stopped and wiped"
