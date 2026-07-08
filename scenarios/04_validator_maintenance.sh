#!/bin/bash
# SCENARIO 05: planned maintenance of a1, stake drained before shutdown so consensus never notices.
set -e
cd "$(dirname "$0")/.." || exit 1

echo
printf '\033[1m==== 🔄 RESET: everyone up, a1-a4 validators, site B spare ====\033[0m\n'
echo
./fleet up 1 2 3 4 5 6 7 8 9 10 11 12
./fleet weight validator 1 2 3 4
./fleet weight spare 7 8 9 10

echo
printf "\033[1m==== 🔧 MAINTENANCE: drain a1's stake, then power it off ====\033[0m\n"
echo
./fleet weight dead 1
./fleet down 1

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
