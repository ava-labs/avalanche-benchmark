#!/usr/bin/env bash

remote_dir="${remote_dir:-/data/avalanche-benchmark}"
env_file="${env_file:-$repo_root/.env}"

require_env_file() {
  if [[ ! -f "$env_file" ]]; then
    echo "ERROR: missing $env_file" >&2
    exit 1
  fi
}

read_env_from_file() {
  local file="$1"
  local key="$2"
  local value
  value="$(awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print }' "$file" | tail -n 1)"
  value="${value%\"}"
  value="${value#\"}"
  value="${value%\'}"
  value="${value#\'}"
  printf '%s' "$value"
}

read_env() {
  read_env_from_file "$env_file" "$1"
}

require_env_from_file() {
  local file="$1"
  local key="$2"
  local value
  value="$(read_env_from_file "$file" "$key")"
  if [[ -z "$value" ]]; then
    echo "ERROR: $file must set $key" >&2
    exit 1
  fi
  printf '%s' "$value"
}

require_env() {
  require_env_from_file "$env_file" "$1"
}

csv_env_values() {
  local key="$1"
  local raw item
  local -a items
  raw="$(read_env "$key")"
  IFS=',' read -r -a items <<< "$raw"
  for item in "${items[@]}"; do
    item="${item#"${item%%[![:space:]]*}"}"
    item="${item%"${item##*[![:space:]]}"}"
    if [[ -n "$item" ]]; then
      printf '%s\n' "$item"
    fi
  done
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

host_work_dir() {
  if is_local_host "$1"; then
    printf '%s' "$repo_root"
  else
    printf '%s' "$remote_dir"
  fi
}

ssh_remote() {
  local host="$1"
  local ssh_user
  ssh_user="$(require_env SSH_USER)"
  printf '%s@%s' "$ssh_user" "$host"
}

ssh_args() {
  local ssh_key
  ssh_key="$(require_env SSH_KEY)"
  printf '%s\0' \
    -F /dev/null \
    -i "$ssh_key" \
    -o ControlMaster=no \
    -o ControlPath=none \
    -o UserKnownHostsFile=/dev/null \
    -o IdentityAgent=none \
    -o IdentitiesOnly=yes \
    -o BatchMode=yes \
    -o ConnectTimeout=8 \
    -o ConnectionAttempts=1 \
    -o ServerAliveInterval=5 \
    -o ServerAliveCountMax=1 \
    -o StrictHostKeyChecking=no
}

load_ssh_args() {
  ssh_args_array=()
  while IFS= read -r -d '' arg; do
    ssh_args_array+=("$arg")
  done < <(ssh_args)
}

run_host_command() {
  local host="$1"
  local command_timeout="$2"
  local command="$3"

  if is_local_host "$host"; then
    if ! timeout "$command_timeout" bash -lc "$command"; then
      echo "ERROR: local command failed or timed out on $host" >&2
      exit 1
    fi
    return
  fi

  load_ssh_args
  local remote
  remote="$(ssh_remote "$host")"
  if ! timeout "$command_timeout" ssh "${ssh_args_array[@]}" "$remote" "$command"; then
    echo "ERROR: SSH command failed or timed out on $remote" >&2
    exit 1
  fi
}

run_host_script() {
  local host="$1"
  local command_timeout="$2"
  local script
  script="$(cat)"

  if is_local_host "$host"; then
    if ! timeout "$command_timeout" bash -s <<< "$script"; then
      echo "ERROR: local script failed or timed out on $host" >&2
      exit 1
    fi
    return
  fi

  load_ssh_args
  local remote
  remote="$(ssh_remote "$host")"
  if ! timeout "$command_timeout" ssh "${ssh_args_array[@]}" "$remote" bash -s <<< "$script"; then
    echo "ERROR: SSH script failed or timed out on $remote" >&2
    exit 1
  fi
}

copy_paths_to_host_dir() {
  local host="$1"
  local dest="$2"
  shift 2

  if is_local_host "$host"; then
    mkdir -p "$dest"
    cp -R "$@" "$dest/"
    return
  fi

  load_ssh_args
  local remote
  remote="$(ssh_remote "$host")"
  if ! timeout 20s ssh "${ssh_args_array[@]}" "$remote" "mkdir -p '$dest'"; then
    echo "ERROR: could not create $remote:$dest" >&2
    exit 1
  fi
  if ! timeout 180s scp "${ssh_args_array[@]}" -r "$@" "$remote:$dest/"; then
    echo "ERROR: SCP failed while copying to $remote:$dest" >&2
    exit 1
  fi
}
