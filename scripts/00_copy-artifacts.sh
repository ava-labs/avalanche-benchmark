#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"

remote_dir="/data/avalanche-benchmark"
env_file="$repo_root/.env"

source "$script_dir/lib.sh"

artifacts=(
  "benchctl"
  "avalanchego"
  "srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"
  "config/genesis.json"
  "config/chain-config.json"
  "config/node-config.json"
  "scripts/00_copy-artifacts.sh"
  "scripts/01_start-pchain.sh"
  "scripts/02_create-l1.sh"
  "scripts/lib.sh"
  ".env"
)

require_artifacts() {
  local missing=0
  for artifact in "${artifacts[@]}"; do
    if [[ ! -f "$repo_root/$artifact" ]]; then
      echo "ERROR: missing $repo_root/$artifact" >&2
      missing=1
    fi
  done
  if [[ "$missing" -ne 0 ]]; then
    echo "Run this from an unpacked runtime package, or run 'make' first in the source checkout." >&2
    exit 1
  fi
}

require_env_file

require_artifacts

benchmark_host_ip="$(require_env BENCHMARK_HOST_IP)"

if is_local_host "$benchmark_host_ip"; then
  echo "Benchmark host is local ($benchmark_host_ip); skipped SSH/SCP."
  exit 0
fi

ssh_user="$(require_env SSH_USER)"
ssh_key="$(require_env SSH_KEY)"
remote="$ssh_user@$benchmark_host_ip"
ssh_args=(
  -F /dev/null
  -i "$ssh_key"
  -o ControlMaster=no
  -o ControlPath=none
  -o IdentityAgent=none
  -o IdentitiesOnly=yes
  -o BatchMode=yes
  -o ConnectTimeout=8
  -o ConnectionAttempts=1
  -o ServerAliveInterval=5
  -o ServerAliveCountMax=1
  -o StrictHostKeyChecking=accept-new
)

run_ssh() {
  if ! timeout 20s ssh "${ssh_args[@]}" "$remote" "$@"; then
    echo "ERROR: SSH command failed or timed out on benchmark host $remote" >&2
    exit 1
  fi
}

run_scp() {
  if ! timeout 120s scp "${ssh_args[@]}" "$@"; then
    echo "ERROR: SCP failed or timed out while copying artifacts to $remote" >&2
    exit 1
  fi
}

remote_tmp="$remote_dir/.upload-$(date +%s)-$$"

run_ssh "rm -rf '$remote_tmp' && mkdir -p '$remote_tmp/config' '$remote_tmp/scripts'"
run_scp \
  "$repo_root/benchctl" \
  "$repo_root/avalanchego" \
  "$repo_root/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy" \
  "$repo_root/.env" \
  "$remote:$remote_tmp/"
run_scp "$repo_root/config/"*.json "$remote:$remote_tmp/config/"
run_scp "$repo_root/scripts/"*.sh "$remote:$remote_tmp/scripts/"
run_ssh "
  set -e
  mkdir -p '$remote_dir/config' '$remote_dir/scripts'
  mv -f '$remote_tmp/benchctl' '$remote_dir/benchctl'
  mv -f '$remote_tmp/avalanchego' '$remote_dir/avalanchego'
  mv -f '$remote_tmp/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy' '$remote_dir/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy'
  mv -f '$remote_tmp/.env' '$remote_dir/.env'
  mv -f '$remote_tmp/config/'*.json '$remote_dir/config/'
  mv -f '$remote_tmp/scripts/'*.sh '$remote_dir/scripts/'
  rm -rf '$remote_tmp'
"

echo "Copied artifacts to benchmark host: $remote:$remote_dir"
