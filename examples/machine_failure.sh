#!/bin/bash
# EXAMPLE: one validator box dies, its hot spare takes over.
#
# Machine numbers assume the default two-site shape (3 validators + 1 spare +
# 2 RPCs per site): m1-m3 site A validators, m4 site A hot spare.
#
# A failure is handled on two independent axes:
#
#   1. hardware:  ./fleet down 1   hard-kills m1 and records the cordon.
#                 Here it SIMULATES the failure; on a real outage the box is
#                 already dead and this just records that fact. It never
#                 touches on-chain weight, so it works even when Fuji is slow.
#
#   2. weight:    ONE seesaw promotes the spare m4 to the validator tier and
#                 drops m1 to dead. One invocation = one converge with raises
#                 landing before lowers, so quorum never dips. Splitting it
#                 into two runs fights the 20% churn budget instead.
set -e
cd "$(dirname "$0")/.." || exit 1

./fleet down 1
./fleet weight validator 4 dead 1
./fleet status

# Recovery once m1's hardware is fixed:
#   ./fleet up 1                        rebuild from genesis and start (weight untouched)
#   ./fleet weight validator 1 spare 4  move consensus back, spare returns to standby
