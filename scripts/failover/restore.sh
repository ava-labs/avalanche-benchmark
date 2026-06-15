#!/bin/bash
# Graceful rolling restore (two-site mode): migrate the validator set onto the
# given site one validator at a time, keeping the chain at >=2/3 throughout — no
# chain downtime, no fork. Brings the target site up as trackers, waits for it to
# sync to the live tip, then rolls each validator key over with a health gate
# between steps. Typical use: restore the original site after ./site-failover.sh.
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
