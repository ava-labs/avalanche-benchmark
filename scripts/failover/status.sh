#!/bin/bash
# Read-only health report: shows each pool node's ACTUAL state (SERVING@block /
# BOOTSTRAPPING / DOWN / cordoned) and an honest validators-serving count.
# Changes nothing. Usage: ./status.sh
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
source "$SCRIPT_DIR/_failover_common.sh"

exec "$RECONCILE_BIN" status
