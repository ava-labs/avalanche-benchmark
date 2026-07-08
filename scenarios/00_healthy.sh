#!/bin/bash
# SCENARIO 00: healthy baseline. Brings the whole fleet to the ground state:
# all machines running, machines 1-3 validating, everything else spare.
# Also the recovery step after any other scenario.
set -e
cd "$(dirname "$0")/.." || exit 1

./scenarios/_ground.sh
./fleet status
