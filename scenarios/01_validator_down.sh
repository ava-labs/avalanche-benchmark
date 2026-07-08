#!/bin/bash
# SCENARIO 01: one validator dies, no replacement promoted.
set -e
cd "$(dirname "$0")/.." || exit 1

# Reset to normal: everyone up, a1-a4 validators, site B spare.
./fleet up 1 2 3 4 5 6 7 8 9 10 11 12
./fleet weight validator 1 2 3 4
./fleet weight spare 7 8 9 10

# Scenario: kill machine 1, drop its stake; chain rides on 3 of 4.
./fleet down 1
./fleet weight dead 1
./fleet status
