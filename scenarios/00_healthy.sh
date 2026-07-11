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
./fleet up 1 2 3 4 5 6 7 8 9 10 11 12
./fleet weight validator 1 2 3 4
./fleet weight spare 7 8 9 10

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
