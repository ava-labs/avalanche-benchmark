#!/bin/bash
# SCENARIO 01: one validator dies, no replacement promoted.
set -e
cd "$(dirname "$0")/.." || exit 1

./scenarios/00_healthy.sh

echo
printf '\033[1m==== 💥 SCENARIO: killing machine 1, chain rides on 3 of 4 ====\033[0m\n'
echo
./fleet down 1
./fleet weight dead 1

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
