#!/bin/bash
# SCENARIO 04: site A comes back after the datacenter failure, consensus moves home.
set -e
cd "$(dirname "$0")/.." || exit 1

echo
printf '\033[1m==== 🔄 RESET: post-failover state, site A dead, site B validating ====\033[0m\n'
echo
./fleet up 1 2 3 4 5 6 7 8 9 10 11 12
./fleet weight validator 1 2 3 4
./fleet weight spare 7 8 9 10
./fleet down 1 2 3 4 5 6
./fleet weight validator 7 8 9
./fleet weight dead 1 2 3 4

echo
printf '\033[1m==== 🏠 FAILBACK: reviving site A (beacons off RPCs 5-6), consensus moves home ====\033[0m\n'
echo
./fleet up 1 2 3 4 5 6
./fleet weight validator 1 2 3 4
./fleet weight spare 7 8 9 10

echo
printf '\033[1m==== 📊 STATUS ====\033[0m\n'
echo
./fleet status
