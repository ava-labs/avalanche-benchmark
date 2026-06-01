#!/bin/bash
# Uncordon a pool machine (return it to service) and reconcile: it rejoins as the
# new spare, or covers an orphaned validator key if quorum was short.
# Usage: ./up.sh <machine 1-4>
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
source "$SCRIPT_DIR/_failover_common.sh"

if [ -z "$1" ]; then
    echo "usage: $0 <machine 1-4>" >&2
    exit 2
fi

exec "$RECONCILE_BIN" up "$1"
