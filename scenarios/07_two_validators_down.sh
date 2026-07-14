#!/bin/bash
# SCENARIO 07: two validators die at once, chain halts by design, P-chain weight ops revive it.
set -e
trap 'printf "\033[1;31m==== ❌ SCENARIO ABORTED (exit %s) ====\033[0m\n" "$?" >&2' ERR
cd "$(dirname "$0")/.." || exit 1

./scenarios/00_healthy.sh

echo
printf '\033[1m==== 💥 SCENARIO: killing a1 and a2, half the stake gone ====\033[0m\n'
echo
./fleet down a1 a2
echo "Chain is now HALTED BY DESIGN: 50% connected stake is below the query gate, block production has stopped."

echo
printf '\033[1m==== 🚑 RECOVERY: drain the dead validators through the P-chain ====\033[0m\n'
echo
./bin/l1 apply --weights a1=1,a2=1
echo "Weight txs are self-signed with the local BLS keys and submitted straight to the P-chain, so they land even while the L1 is halted; the chain resumes once the txs are accepted."

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
