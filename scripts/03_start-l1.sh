#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"

remote_dir="/data/avalanche-benchmark"
env_file="$repo_root/.env"
l1_env_file="$repo_root/runtime-data/l1.env"
node_ids_file="$repo_root/staking/node-ids.env"
subnet_config_file="$repo_root/config/subnet-config.json"
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

  if [[ "$sybil_enabled_local" -eq 1 ]]; then
    while IFS= read -r host; do
      if [[ -n "${seen[$host]:-}" ]]; then
        echo "ERROR: duplicate node host in DC1_NODE_IPS: $host" >&2
        exit 1
      fi
      seen[$host]=1
      node_hosts+=("$host")
      if [[ "${#node_hosts[@]}" -eq "$l1_validator_count" ]]; then
        break
      fi
    done < <(csv_env_values DC1_NODE_IPS)

    if [[ "${#node_hosts[@]}" -lt "$l1_validator_count" ]]; then
      echo "ERROR: SYBIL_ENABLED_LOCAL needs at least $l1_validator_count DC1_NODE_IPS, got ${#node_hosts[@]}" >&2
      exit 1
    fi
    return
  fi

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
  local ips=()

  if [[ "$sybil_enabled_local" -eq 1 ]]; then
    for j in "${!primary_node_hosts[@]}"; do
      ips+=("${primary_node_hosts[$j]}:9651")
    done
  else
    ips=("$benchmark_host_ip:9651" "$benchmark_host_ip:9653")
  fi

  for j in "${!node_hosts[@]}"; do
    if [[ "$j" == "$current_index" ]]; then
      continue
    fi
    ips+=("${node_hosts[$j]}:$l1_staking_port")
  done

  join_by_comma "${ips[@]}"
}

bootstrap_ids_for_node() {
  local current_index="$1"
  local ids=()

  if [[ "$sybil_enabled_local" -eq 1 ]]; then
    for j in 1 2 3 4 5; do
      ids+=("$(require_env_from_file "$node_ids_file" "L1_${j}_NODE_ID")")
    done
  else
    ids=("$pchain_1_node_id" "$pchain_2_node_id")
  fi

  for j in "${!l1_node_ids[@]}"; do
    if [[ "$j" == "$current_index" ]]; then
      continue
    fi
    ids+=("${l1_node_ids[$j]}")
  done

  join_by_comma "${ids[@]}"
}

l1_identity_index_for_node_index() {
  local zero_based_index="$1"
  printf '%s' "$((l1_validator_start_index + zero_based_index))"
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
  local info_rpc="http://$host:$l1_http_port"
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
l1_validator_count="$(require_env_from_file "$l1_env_file" L1_VALIDATOR_COUNT)"
l1_validator_start_index="$(read_env_from_file "$l1_env_file" L1_VALIDATOR_START_INDEX)"
if [[ -z "$l1_validator_start_index" ]]; then
  l1_validator_start_index=1
fi
if ! [[ "$l1_validator_count" =~ ^[0-9]+$ ]] || [[ "$l1_validator_count" -lt 1 ]]; then
  echo "ERROR: $l1_env_file must set L1_VALIDATOR_COUNT to a positive integer" >&2
  exit 1
fi
if ! [[ "$l1_validator_start_index" =~ ^[0-9]+$ ]] || [[ "$l1_validator_start_index" -lt 1 ]]; then
  echo "ERROR: $l1_env_file must set L1_VALIDATOR_START_INDEX to a positive integer" >&2
  exit 1
fi

sybil_enabled_local=0
if truthy "$(read_env SYBIL_ENABLED_LOCAL)"; then
  sybil_enabled_local=1
fi
l1_http_port=9650
l1_staking_port=9651
if [[ "$sybil_enabled_local" -eq 1 ]]; then
  l1_http_port=9652
  l1_staking_port=9653
fi

collect_node_hosts

primary_node_hosts=()
if [[ "$sybil_enabled_local" -eq 1 ]]; then
  mapfile -t primary_node_hosts < <(csv_env_values DC1_NODE_IPS | head -n 5)
  if [[ "${#primary_node_hosts[@]}" -lt 5 ]]; then
    echo "ERROR: SYBIL_ENABLED_LOCAL needs at least five DC1_NODE_IPS for primary validators" >&2
    exit 1
  fi
fi

for node_host in "${node_hosts[@]}"; do
  if [[ "$node_host" == "$benchmark_host_ip" ]]; then
    echo "ERROR: BENCHMARK_HOST_IP must not appear in DC node IPs: $node_host" >&2
    exit 1
  fi
done

l1_node_ids=()
for i in "${!node_hosts[@]}"; do
  identity_index="$(l1_identity_index_for_node_index "$i")"
  for file in signer.key staker.crt staker.key; do
    require_file "$repo_root/staking/l1/$identity_index/$file"
  done
  l1_node_ids+=("$(require_env_from_file "$node_ids_file" "L1_${identity_index}_NODE_ID")")
done

for i in "${!node_hosts[@]}"; do
  node_index=$((i + 1))
  node_host="${node_hosts[$i]}"

  echo "Stopping l1-$node_index on $node_host"
  run_host_script "$node_host" 20s <<SCRIPT
set -euo pipefail
pkill -TERM -f 'runtime-data/l1' || true
sleep 2
pkill -KILL -f 'runtime-data/l1' || true
SCRIPT
done

for i in "${!node_hosts[@]}"; do
  node_index=$((i + 1))
  node_host="${node_hosts[$i]}"
  node_work_dir="$(host_work_dir "$node_host")"
  node_data_dir="$node_work_dir/runtime-data/l1"

  echo "Preparing l1-$node_index on $node_host"
  run_host_script "$node_host" 45s <<SCRIPT
set -euo pipefail
cd '$node_work_dir'
rm -rf runtime-data/l1
mkdir -p \
  runtime-data/l1/staking \
  runtime-data/l1/plugins \
  runtime-data/l1/configs/chains/$l1_chain_id \
  runtime-data/l1/configs/subnets
test -x bin/avalanchego
test -x plugins/$plugin_id
cp -f plugins/$plugin_id runtime-data/l1/plugins/$plugin_id
chmod +x runtime-data/l1/plugins/$plugin_id
SCRIPT

  copy_paths_to_host_dir "$node_host" "$node_data_dir/staking" \
    "$repo_root/staking/l1/$(l1_identity_index_for_node_index "$i")/signer.key" \
    "$repo_root/staking/l1/$(l1_identity_index_for_node_index "$i")/staker.crt" \
    "$repo_root/staking/l1/$(l1_identity_index_for_node_index "$i")/staker.key"

  copy_paths_to_host_dir "$node_host" "$node_data_dir/configs/chains/$l1_chain_id" \
    "$repo_root/config/chain-config.json"
  run_host_command "$node_host" 20s "mv -f '$node_data_dir/configs/chains/$l1_chain_id/chain-config.json' '$node_data_dir/configs/chains/$l1_chain_id/config.json'"

  if [[ -f "$subnet_config_file" ]]; then
    copy_paths_to_host_dir "$node_host" "$node_data_dir/configs/subnets" "$subnet_config_file"
    run_host_command "$node_host" 20s "mv -f '$node_data_dir/configs/subnets/subnet-config.json' '$node_data_dir/configs/subnets/$l1_subnet_id.json'"
  fi
done

for i in "${!node_hosts[@]}"; do
  node_index=$((i + 1))
  node_host="${node_hosts[$i]}"
  node_work_dir="$(host_work_dir "$node_host")"
  node_data_dir="$node_work_dir/runtime-data/l1"
  bootstrap_ips="$(bootstrap_ips_for_node "$i")"
  bootstrap_ids="$(bootstrap_ids_for_node "$i")"
  sybil_flag="$sybil_enabled_local"

  run_host_script "$node_host" 30s <<SCRIPT
set -euo pipefail
cd '$node_work_dir'
extra_args=()
if [[ "$sybil_flag" == "1" ]]; then
  extra_args+=(--http-port=9652 --staking-port=9653)
else
  extra_args+=(--sybil-protection-enabled=false)
fi
nohup bin/avalanchego \
  --data-dir=runtime-data/l1 \
  --network-id=local \
  --http-host=0.0.0.0 \
  --public-ip=$node_host \
  "\${extra_args[@]}" \
  --track-subnets=$l1_subnet_id \
  --bootstrap-ips=$bootstrap_ips \
  --bootstrap-ids=$bootstrap_ids \
  > runtime-data/l1/stdout.log 2>&1 &
disown || true
echo "started l1-$node_index pid=\$! rpc=http://$node_host:$l1_http_port log=$node_data_dir/stdout.log"
SCRIPT
done

for i in "${!node_hosts[@]}"; do
  node_index=$((i + 1))
  node_host="${node_hosts[$i]}"
  expected_node_id="$(require_env_from_file "$node_ids_file" "L1_$(l1_identity_index_for_node_index "$i")_NODE_ID")"
  wait_l1_ready "$node_host" "$node_index" "$expected_node_id"
done

echo "L1 nodes ready"
for i in "${!node_hosts[@]}"; do
  node_index=$((i + 1))
  node_host="${node_hosts[$i]}"
  node_work_dir="$(host_work_dir "$node_host")"
  identity_index="$(l1_identity_index_for_node_index "$i")"
  expected_node_id="$(require_env_from_file "$node_ids_file" "L1_${identity_index}_NODE_ID")"
  echo "  l1-$node_index identity=l1/$identity_index RPC: http://$node_host:$l1_http_port/ext/bc/$l1_chain_id/rpc expectedNodeID=$expected_node_id log=$node_work_dir/runtime-data/l1/stdout.log"
done
