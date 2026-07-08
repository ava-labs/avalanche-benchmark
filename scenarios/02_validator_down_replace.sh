#!/bin/bash
# SCENARIO 02: one validator dies, a site B machine takes over.
set -e
cd "$(dirname "$0")/.." || exit 1

# Reset to normal: everyone up, a1-a4 validators, site B spare.
./scenarios/_ground.sh

# Scenario: kill machine 1, promote machine 7 in its place.
./fleet down 1
./fleet weight validator 7 dead 1
./fleet status
