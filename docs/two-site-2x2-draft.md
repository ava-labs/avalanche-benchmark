# Draft 2x2 Two-Site Topology

This draft does not use the failover controller path. It deploys one active L1
validator set split across two regions:

- site A: 2 validators, 1 pinned non-validating RPC tracker
- site B: 2 validators, 1 pinned non-validating RPC tracker
- remaining existing machines: non-validating spare or RPC trackers, included so
  no old process keeps a duplicate staking identity alive

Provisioning is intentionally outside this draft. Reuse the existing machine
fleet without running Terraform. Put two site A machines and two site B machines
in `VALIDATOR_IPS`, and put the non-validating tracker machines in `RPC_IPS`.
Leave every `BACKUP_*` variable unset, because this is not the failover
topology.

```bash
SSH_USER=ubuntu
SSH_KEY_PATH=/path/to/fleet-key
REMOTE_DIR=~/avalanche-benchmark

# A1,A2 are site A validators. B1,B2 are site B validators.
VALIDATOR_IPS=A1,A2,B1,B2
SPARE_IPS=

# The pinned non-validating RPC trackers. Include the existing RPC machines from
# both sites when reusing the full current fleet.
RPC_IPS=A_RPC1,A_RPC2,B_RPC1,B_RPC2
```

Consensus parameters in `subnet-config.json` are sized for four validators:
`k=4`, `alphaPreference=3`, `alphaConfidence=4`, `beta=25`.

Run the normal deploy and benchmark sequence from the control host:

```bash
./01_bootstrap_primary_network.sh
./02_create_l1.sh
./03_wipe_and_deploy_l1.sh
./05_benchmark.sh
```
