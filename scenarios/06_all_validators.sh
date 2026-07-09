#!/bin/bash
# SCENARIO 06: all eight stake slots validating, four per datacenter.
set -e
cd "$(dirname "$0")/.." || exit 1

./scenarios/00_healthy.sh

echo
printf '\033[1m==== ⚖️ WEIGHTS: all eight stake slots to validator ====\033[0m\n'
echo
./fleet weight validator 1 2 3 4 7 8 9 10

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
