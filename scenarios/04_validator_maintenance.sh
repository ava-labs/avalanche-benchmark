#!/bin/bash
# SCENARIO 04: planned maintenance of a1, stake drained before shutdown so consensus never notices.
set -e
cd "$(dirname "$0")/.." || exit 1

./scenarios/00_healthy.sh

echo
printf "\033[1m==== 🔧 MAINTENANCE: drain a1's stake, then power it off ====\033[0m\n"
echo
./fleet weight dead 1
./fleet down 1

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
