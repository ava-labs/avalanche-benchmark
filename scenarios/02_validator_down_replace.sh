#!/bin/bash
# SCENARIO 02: one validator dies, a standby-site machine takes over.
#
# Machine 1 is killed; site A has no spare left (all four site A boxes
# validate in the ground state), so ONE weight command promotes machine 7
# (b1, the standby site) to the validator tier and drops machine 1 to dead
# (raise before lower, so quorum never dips). Result: 4 active validators
# again, full block production speed.
set -e
cd "$(dirname "$0")/.." || exit 1

./scenarios/_ground.sh

./fleet down 1
./fleet weight validator 7 dead 1
./fleet status

# Back to healthy: ./scenarios/00_healthy.sh
