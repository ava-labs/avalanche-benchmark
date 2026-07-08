#!/bin/bash
# Ground state shared by every scenario: all machines up, 1-4 validators, 7-10 spare.
set -e
cd "$(dirname "$0")/.." || exit 1

# Bring every machine up (idempotent).
./fleet up $(./fleet endpoints | awk '{print NR}')

# Restore default stake tiers.
./fleet weight validator 1 2 3 4 spare 7 8 9 10

# Wait for all 4 validators to serve.
deadline=$((SECONDS + 600))
while ! ./fleet status | grep -q 'validators serving: 4/4'; do
    if ((SECONDS >= deadline)); then
        echo "ground: timed out waiting for 4/4 validators serving" >&2
        exit 1
    fi
    sleep 5
done
echo "ground: 4/4 validators serving"
