#!/bin/bash
# SCENARIO 04: site A comes back after scenario 03, consensus moves home. Run after 03.
set -e
cd "$(dirname "$0")/.." || exit 1

echo
printf '\033[1m==== 🔄 FAILBACK: reviving site A ====\033[0m\n'
echo
./fleet up 1 2 3 4 5 6

echo
printf '\033[1m==== ⚖️ WEIGHTS: consensus moves home, site B stands down ====\033[0m\n'
echo
./fleet weight validator 1 2 3 4
./fleet weight spare 7 8 9 10

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
