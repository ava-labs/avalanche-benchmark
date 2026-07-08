#!/bin/bash
# Shared ground state, called by every numbered scenario. Idempotent:
#   1. bring every machine up (`fleet up` skips machines already serving)
#   2. restore the default stake tiers in ONE weight command (raises land
#      before lowers, so quorum never dips mid-move)
#   3. wait until all 3 validators are serving
#
# Default two-site shape: machines 1-3 site A validators, 4 site A spare,
# 5-6 site A RPCs, 7-10 site B spares, 11-12 site B RPCs.
set -e
cd "$(dirname "$0")/.." || exit 1

# One machine per endpoints line; up is idempotent, healthy nodes are skipped.
./fleet up $(./fleet endpoints | awk '{print NR}')
./fleet weight validator 1 2 3 spare 4 7 8 9 10

# A rebuilt validator resyncs the chain before it serves; first boot can take
# minutes, so poll with a generous timeout.
deadline=$((SECONDS + 600))
while ! ./fleet status | grep -q 'validators serving: 3/3'; do
    if ((SECONDS >= deadline)); then
        echo "ground: timed out waiting for 3/3 validators serving" >&2
        exit 1
    fi
    sleep 5
done
echo "ground: 3/3 validators serving"
