#!/bin/bash
# SCENARIO 00: healthy baseline, also the recovery step after any scenario.
set -e
cd "$(dirname "$0")/.." || exit 1

# Reset to normal: everyone up, a1-a4 validators, site B spare.
./scenarios/_ground.sh
./fleet status
