#!/bin/bash
# SCENARIO 03: primary datacenter (site A) goes dark, site B takes over.
set -e
cd "$(dirname "$0")/.." || exit 1

# Reset to normal: everyone up, a1-a4 validators, site B spare.
./scenarios/_ground.sh

# Scenario: kill all of site A; site B validators take consensus.
./fleet down 1 2 3 4 5 6
./fleet weight validator 7 8 9 dead 1 2 3 4
./fleet status
