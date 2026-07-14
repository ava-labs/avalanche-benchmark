#!/bin/bash
# SCENARIO 00: healthy baseline, also the recovery step after any scenario.
set -e
# set -e aborts on the first failed fleet command; the trap makes that abort
# LOUD so a wrapping loop racing to the next scenario cannot hide it.
trap 'printf "\033[1;31m==== ❌ RESET FAILED (exit %s): fleet left unhealthy, scenario aborted ====\033[0m\n" "$?" >&2' ERR
cd "$(dirname "$0")/.." || exit 1

echo
printf '\033[1m==== 🔄 RESET: everyone up, a1-a4 validators, site B spare ====\033[0m\n'
echo
./fleet up a1 a2 a3 a4 rpc_a1 rpc_a2 b1 b2 b3 b4 rpc_b1 rpc_b2
./bin/l1 apply --weights a1=100000,a2=100000,a3=100000,a4=100000,b1=1000,b2=1000,b3=1000,b4=1000

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
