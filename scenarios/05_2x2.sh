#!/bin/bash
# SCENARIO 05: 2x2 split, two validators in each datacenter.
set -e
trap 'printf "\033[1;31m==== ❌ SCENARIO ABORTED (exit %s) ====\033[0m\n" "$?" >&2' ERR
cd "$(dirname "$0")/.." || exit 1

./scenarios/00_healthy.sh

echo
printf '\033[1m==== ⚖️ WEIGHTS: two validators per DC ====\033[0m\n'
echo
./bin/l1 apply --weights a1=100000,a2=100000,b1=100000,b2=100000,a3=1000,a4=1000,b3=1000,b4=1000

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
