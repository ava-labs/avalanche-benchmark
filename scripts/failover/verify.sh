#!/bin/bash
# Read-only proof that the live network is on a SINGLE branch (no fork) and that
# quorum is healthy. Compares the finalized block hash at a common height across
# every live node. Changes nothing.
# Usage: ./verify.sh
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
source "$SCRIPT_DIR/_failover_common.sh"

exec "$RECONCILE_BIN" verify
