#!/usr/bin/env bash

remote_dir="${remote_dir:-/data/avalanche-benchmark}"
env_file="${env_file:-$repo_root/.env}"
benchmark_command_timeout="${BENCHMARK_COMMAND_TIMEOUT:-10m}"

require_env_file() {
  if [[ ! -f "$env_file" ]]; then
    echo "ERROR: missing $env_file" >&2
    exit 1
  fi
}

read_env() {
  local key="$1"
  local value
  value="$(awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print }' "$env_file" | tail -n 1)"
  value="${value%\"}"
  value="${value#\"}"
  value="${value%\'}"
  value="${value#\'}"
  printf '%s' "$value"
}

require_env() {
  local key="$1"
  local value
  value="$(read_env "$key")"
  if [[ -z "$value" ]]; then
    echo "ERROR: $env_file must set $key" >&2
    exit 1
  fi
  printf '%s' "$value"
}

is_local_host() {
  case "$1" in
    127.0.0.1|localhost|::1)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

run_on_benchmark_host() {
  require_env_file

  local benchmark_host_ip
  benchmark_host_ip="$(require_env BENCHMARK_HOST_IP)"

  if is_local_host "$benchmark_host_ip"; then
    cd "$repo_root"
    "$@"
    return
  fi

  local ssh_user ssh_key remote
  ssh_user="$(require_env SSH_USER)"
  ssh_key="$(require_env SSH_KEY)"
  remote="$ssh_user@$benchmark_host_ip"

  local remote_cmd
  remote_cmd="$(printf "%q " "$@")"

  if ! timeout "$benchmark_command_timeout" ssh \
    -F /dev/null \
    -i "$ssh_key" \
    -o ControlMaster=no \
    -o ControlPath=none \
    -o IdentityAgent=none \
    -o IdentitiesOnly=yes \
    -o BatchMode=yes \
    -o ConnectTimeout=8 \
    -o ConnectionAttempts=1 \
    -o ServerAliveInterval=5 \
    -o ServerAliveCountMax=1 \
    -o StrictHostKeyChecking=accept-new \
    "$remote" "cd '$remote_dir' && $remote_cmd"; then
    echo "ERROR: SSH command failed or timed out on benchmark host $remote" >&2
    exit 1
  fi
}
