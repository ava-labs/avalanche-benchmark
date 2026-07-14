#!/bin/bash
# SCENARIO 01: one validator dies, no replacement promoted.
set -e
trap 'printf "\033[1;31m==== ❌ SCENARIO ABORTED (exit %s) ====\033[0m\n" "$?" >&2' ERR
cd "$(dirname "$0")/.." || exit 1

./scenarios/00_healthy.sh

echo
printf '\033[1m==== 💥 SCENARIO: killing a1, chain rides on 3 of 4 ====\033[0m\n'
echo
./fleet down a1
./bin/l1 set-weight --node a1 --weight 1

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
