#!/bin/bash
# SCENARIO 07: two validators die at once, chain halts by design, P-chain weight ops revive it.
set -e
cd "$(dirname "$0")/.." || exit 1

echo
printf '\033[1m==== 🔄 RESET: everyone up, a1-a4 validators, site B spare ====\033[0m\n'
echo
./fleet up 1 2 3 4 5 6 7 8 9 10 11 12
./fleet weight validator 1 2 3 4
./fleet weight spare 7 8 9 10

echo
printf '\033[1m==== 💥 SCENARIO: killing machines 1 and 2, half the stake gone ====\033[0m\n'
echo
./fleet down 1 2
echo "Chain is now HALTED BY DESIGN: 50% connected stake is below the query gate, block production has stopped."

echo
printf '\033[1m==== 🚑 RECOVERY: drain the dead validators through the P-chain ====\033[0m\n'
echo
./fleet weight dead 1 2
echo "Weight changes ride the primary-network warp pipeline, so they land even while the L1 is halted; the chain resumes once the P-chain txs are accepted."

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
