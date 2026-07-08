#!/bin/bash
# SCENARIO 00: healthy baseline, also the recovery step after any scenario.
set -e
cd "$(dirname "$0")/.." || exit 1

# Reset to normal: everyone up, a1-a4 validators, site B spare.
./fleet up 1 2 3 4 5 6 7 8 9 10 11 12
./fleet weight validator 1 2 3 4
./fleet weight spare 7 8 9 10

./fleet status
