#!/bin/bash
# SCENARIO 03: the whole primary datacenter (site A) goes dark.
#
# Every site A box is killed, including the two site A RPC nodes; the load
# generator keeps pushing transactions through the site B RPCs. ONE weight
# command raises site B's validators (7 8 9) and drops all site A stake to
# dead (raises land before lowers, so the chain never loses quorum and each
# lower fits a single churn step). Result: site B runs consensus, ingress
# survives on site B RPCs.
set -e
cd "$(dirname "$0")/.." || exit 1

./scenarios/_ground.sh

./fleet down 1 2 3 4 5 6
./fleet weight validator 7 8 9 dead 1 2 3 4
./fleet status

# Back to healthy: ./scenarios/00_healthy.sh
