#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/../.." && pwd)"

terraform -chdir="$script_dir" init -input=false
terraform -chdir="$script_dir" apply -input=false -auto-approve "$@"

terraform -chdir="$script_dir" output -raw env > "$repo_root/.env"

echo "Wrote $repo_root/.env"
