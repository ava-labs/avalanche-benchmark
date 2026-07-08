#!/bin/bash
# SCENARIO 02: one validator dies, the hot spare takes over.
#
# Machine 1 is killed; ONE weight command promotes spare machine 4 to the
# validator tier and drops machine 1 to dead (raise before lower, so quorum
# never dips). Result: 3 active validators again, full block production speed.
set -e
cd "$(dirname "$0")/.." || exit 1

./scenarios/_ground.sh

./fleet down 1
./fleet weight validator 4 dead 1
./fleet status

# Back to healthy: ./scenarios/00_healthy.sh
