#!/bin/bash
# SCENARIO 02: one validator dies, a site B machine takes over.
set -e
cd "$(dirname "$0")/.." || exit 1

echo
printf '\033[1m==== 🔄 RESET: everyone up, a1-a4 validators, site B spare ====\033[0m\n'
echo
./fleet up 1 2 3 4 5 6 7 8 9 10 11 12
./fleet weight validator 1 2 3 4
./fleet weight spare 7 8 9 10

echo
printf '\033[1m==== 💥 SCENARIO: killing machine 1, machine 7 takes over ====\033[0m\n'
echo
./fleet down 1
./fleet weight validator 7
./fleet weight dead 1

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
