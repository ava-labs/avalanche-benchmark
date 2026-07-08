#!/bin/bash
# SCENARIO 01: one validator dies, no spare promoted.
#
# Machine 1 is killed and its stake dropped to the dead tier, leaving 2 of 3
# validators. The chain keeps producing (2/3 quorum holds), but roughly a
# third of proposer slots belong to the dead node, so about every third block
# stalls for around a second until the next proposer takes over.
set -e
cd "$(dirname "$0")/.." || exit 1

./scenarios/_ground.sh

./fleet down 1
./fleet weight dead 1
./fleet status

# Back to healthy: ./scenarios/00_healthy.sh
