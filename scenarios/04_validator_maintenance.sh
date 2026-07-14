#!/bin/bash
# SCENARIO 04: planned maintenance of a1, stake drained before shutdown so consensus never notices.
set -e
trap 'printf "\033[1;31m==== ❌ SCENARIO ABORTED (exit %s) ====\033[0m\n" "$?" >&2' ERR
cd "$(dirname "$0")/.." || exit 1

./scenarios/00_healthy.sh

echo
printf "\033[1m==== 🔧 MAINTENANCE: drain a1's stake, then power it off ====\033[0m\n"
echo
./bin/l1 set-weight --node a1 --weight 1
./fleet down 1

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
