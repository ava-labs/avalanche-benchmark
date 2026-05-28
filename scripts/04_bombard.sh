#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"
remote_dir="/data/avalanche-benchmark"
env_file="$repo_root/.env"
l1_env_file="$repo_root/runtime-data/l1.env"

source "$script_dir/lib.sh"

usage() {
  cat >&2 <<'EOF'
Usage:
  ./scripts/04_bombard.sh [--time DURATION] [--starting-tps TPS]

Examples:
  ./scripts/04_bombard.sh --time 40s --starting-tps 1000
  ./scripts/04_bombard.sh
EOF
}

bombard_args=()
has_time=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --time)
      if [[ $# -lt 2 ]]; then
        echo "ERROR: --time requires a value" >&2
        usage
        exit 1
      fi
      bombard_args+=("--time=$2")
      has_time=1
      shift 2
      ;;
    --time=*)
      bombard_args+=("$1")
      has_time=1
      shift
      ;;
    --starting-tps)
      if [[ $# -lt 2 ]]; then
        echo "ERROR: --starting-tps requires a value" >&2
        usage
        exit 1
      fi
      bombard_args+=("--starting-tps=$2")
      shift 2
      ;;
    --starting-tps=*)
      bombard_args+=("$1")
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "ERROR: unknown argument: $1" >&2
      usage
      exit 1
      ;;
  esac
done

require_env_file
if [[ ! -f "$l1_env_file" ]]; then
  echo "ERROR: missing $l1_env_file; run ./scripts/02_create-l1.sh first" >&2
  exit 1
fi

benchmark_host_ip="$(require_env BENCHMARK_HOST_IP)"
validator_count="$(require_env_from_file "$l1_env_file" L1_VALIDATOR_COUNT)"
l1_chain_id="$(require_env_from_file "$l1_env_file" L1_CHAIN_ID)"
l1_rpc_port=9650
if truthy "$(read_env SYBIL_ENABLED_LOCAL)"; then
  l1_rpc_port=9652
fi

if ! [[ "$validator_count" =~ ^[0-9]+$ ]] || [[ "$validator_count" -lt 1 ]]; then
  echo "ERROR: $l1_env_file must set L1_VALIDATOR_COUNT to a positive integer" >&2
  exit 1
fi

mapfile -t node_hosts < <(csv_env_values DC1_NODE_IPS)
if [[ "${#node_hosts[@]}" -lt "$validator_count" ]]; then
  echo "ERROR: DC1_NODE_IPS has ${#node_hosts[@]} hosts, need $validator_count validators" >&2
  exit 1
fi

rpcs=()
for ((i = 0; i < validator_count; i++)); do
  rpcs+=("http://${node_hosts[$i]}:${l1_rpc_port}/ext/bc/${l1_chain_id}/rpc")
done
rpcs_csv="$(IFS=,; printf '%s' "${rpcs[*]}")"

benchmark_work_dir="$(host_work_dir "$benchmark_host_ip")"
bombard_binary="$repo_root/bombard"
if [[ ! -f "$bombard_binary" ]]; then
  echo "ERROR: missing $bombard_binary; run 'make bombard' first" >&2
  exit 1
fi
copy_paths_to_host_dir "$benchmark_host_ip" "$benchmark_work_dir/bin" "$bombard_binary"
run_host_command "$benchmark_host_ip" 10s "chmod +x '$benchmark_work_dir/bin/bombard'"

cmd_args=("--rpcs=$rpcs_csv" "${bombard_args[@]}")

if is_local_host "$benchmark_host_ip"; then
  cd "$benchmark_work_dir"
  exec ./bin/bombard "${cmd_args[@]}"
fi

load_ssh_args
remote="$(ssh_remote "$benchmark_host_ip")"
remote_command="cd $(printf '%q' "$benchmark_work_dir") && exec ./bin/bombard"
for arg in "${cmd_args[@]}"; do
  remote_command+=" $(printf '%q' "$arg")"
done

if [[ "$has_time" -eq 1 ]]; then
  exec ssh "${ssh_args_array[@]}" "$remote" "$remote_command"
fi

exec ssh -tt "${ssh_args_array[@]}" "$remote" "$remote_command"
