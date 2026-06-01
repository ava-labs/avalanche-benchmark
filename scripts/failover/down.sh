#!/bin/bash
# Cordon a pool machine (mark it out of service) and reconcile: its validator
# identity, if any, fails over to the spare. Usage: ./down.sh <machine 1-4>
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
source "$SCRIPT_DIR/_failover_common.sh"

if [ -z "$1" ]; then
    echo "usage: $0 <machine 1-4>" >&2
    exit 2
fi

exec "$RECONCILE_BIN" down "$1"
