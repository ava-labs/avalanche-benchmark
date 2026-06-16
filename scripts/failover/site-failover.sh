#!/bin/bash
# Fail the whole validator set over to the given site (two-site mode only):
# every machine in the other site is cordoned, the target site's syncing
# trackers take over the three validator identities, and the target site's
# pinned RPC becomes the benchmark ingress. Requires BACKUP_SITE_NODE_IPS in
# .env. Usage: ./site-failover.sh <a|b>
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
source "$SCRIPT_DIR/_failover_common.sh"

if [ -z "$1" ]; then
    echo "usage: $0 <a|b>" >&2
    exit 2
fi

exec "$RECONCILE_BIN" site-failover "$1"
