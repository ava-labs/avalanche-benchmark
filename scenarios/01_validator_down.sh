#!/bin/bash
# SCENARIO 01: one validator dies, no replacement promoted.
set -e
cd "$(dirname "$0")/.." || exit 1

# Reset to normal: everyone up, a1-a4 validators, site B spare.
./scenarios/_ground.sh

# Scenario: kill machine 1, drop its stake; chain rides on 3 of 4.
./fleet down 1
./fleet weight dead 1
./fleet status
