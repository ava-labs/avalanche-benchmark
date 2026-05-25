#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/../.." && pwd)"

terraform -chdir="$script_dir" init -input=false
terraform -chdir="$script_dir" destroy -input=false -auto-approve "$@"

rm -f "$repo_root/.env"

echo "Destroyed AWS Terraform stack"
echo "Removed $repo_root/.env"
