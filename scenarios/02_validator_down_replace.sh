#!/bin/bash
# SCENARIO 02: one validator dies, a site B machine takes over.
set -e
cd "$(dirname "$0")/.." || exit 1

./scenarios/00_healthy.sh

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
