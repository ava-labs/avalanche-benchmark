#!/bin/bash
# SCENARIO 02: one validator dies, a site B machine takes over.
set -e
trap 'printf "\033[1;31m==== ❌ SCENARIO ABORTED (exit %s) ====\033[0m\n" "$?" >&2' ERR
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
