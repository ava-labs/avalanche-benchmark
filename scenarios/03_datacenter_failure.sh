#!/bin/bash
# SCENARIO 03: primary datacenter (site A) goes dark, site B takes over.
set -e
cd "$(dirname "$0")/.." || exit 1

./scenarios/00_healthy.sh

echo
printf '\033[1m==== 💥 SCENARIO: killing all of site A ====\033[0m\n'
echo
./fleet down 1 2 3 4 5 6

echo
printf '\033[1m==== ⚖️ WEIGHTS: site B takes consensus ====\033[0m\n'
echo
./fleet weight validator 7 8 9 10
./fleet weight dead 1 2 3 4

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
