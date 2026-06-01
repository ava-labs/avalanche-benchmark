#!/bin/bash
# Pure reconcile: re-apply the existing intentions to reality without changing any
# intent. Use it to recover from an interrupted run (SSH hang, Ctrl-C) or to
# self-heal a crashed-but-uncordoned node. Usage: ./failover.sh
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
source "$SCRIPT_DIR/_failover_common.sh"

exec "$RECONCILE_BIN" apply
