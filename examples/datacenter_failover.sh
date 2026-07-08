#!/bin/bash
# EXAMPLE: the whole primary datacenter (site A) goes dark, site B takes over.
#
# Machine numbers assume the default two-site shape (3 validators + 1 spare +
# 2 RPCs per site): machines 1-6 are site A, 7-12 are site B (7 8 9 = site B
# validators b1-b3).
#
#   1. hardware:  kill every site A box. On a real outage skip this line, the
#                 DC is already gone; `down` here both simulates the outage and
#                 records the cordons.
#
#   2. weight:    ONE seesaw raises site B's validators and drops site A's to
#                 dead. This MUST be a single invocation: the engine orders all
#                 raises before all lowers within one converge, so the L1 never
#                 loses quorum and every lower fits a single 20% churn step.
#                 Two separate runs (lower first, or raise then lower) ratchet
#                 against a shrinking total and take 10x the transactions.
set -e
cd "$(dirname "$0")/.." || exit 1

./fleet down 1 2 3 4 5 6
./fleet weight validator 7 8 9 dead 1 2 3
./fleet status

# Failback once site A is restored:
#   ./fleet up 1 2 3 4 5 6
#   ./fleet weight validator 1 2 3 dead 7 8 9
