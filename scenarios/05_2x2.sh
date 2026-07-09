#!/bin/bash
# SCENARIO 05: 2x2 split, two validators in each datacenter.
set -e
cd "$(dirname "$0")/.." || exit 1

./scenarios/00_healthy.sh

echo
printf '\033[1m==== ⚖️ WEIGHTS: two validators per DC ====\033[0m\n'
echo
./fleet weight validator 1 2 7 8
./fleet weight spare 3 4 9 10

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
