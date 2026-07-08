#!/bin/bash
# SCENARIO 01: one validator dies, no replacement promoted.
#
# Machine 1 is killed and its stake dropped to the dead tier, leaving 3 of 4
# validators. The chain keeps full quorum on 3 of 4: consensus is tuned to
# ride through one lost validator, so block production continues smoothly.
set -e
cd "$(dirname "$0")/.." || exit 1

./scenarios/_ground.sh

./fleet down 1
./fleet weight dead 1
./fleet status

# Back to healthy: ./scenarios/00_healthy.sh
