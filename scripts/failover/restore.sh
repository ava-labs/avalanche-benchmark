#!/bin/bash
# Graceful restore (two-site mode): move the active consensus weight onto the
# given site via the ValidatorManager weight seesaw. No key swaps, no chain
# downtime, no fork. Brings both sites up, waits until the target site serves
# at tip, then raises its validators' weights before lowering the other side's.
# Typical use: restore the original site after ./site-failover.sh.
# Usage: ./restore.sh <a|b>
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
source "$SCRIPT_DIR/_failover_common.sh"

if [ -z "$1" ]; then
    echo "usage: $0 <a|b>" >&2
    exit 2
fi

exec "$RECONCILE_BIN" restore "$1"
