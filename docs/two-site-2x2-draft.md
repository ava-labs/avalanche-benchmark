# 2x2 Two-Data-Center Draft (active-active)

This branch deploys one L1 whose validator set is split across two REAL data
centers, both active:

- **DC A** (`m1..m4`): 2 live validators (`m1`,`m2`) + 2 pinned non-validating
  RPC trackers (`m3`,`m4`)
- **DC B** (`b1..b4`): 2 live validators (`b1`,`b2`) + 2 pinned non-validating
  RPC trackers (`b3`,`b4`)

There is **no failover and no restore** on this branch — that machinery
(`site-failover`, `restore`, snapshot seeding, the backup-site block-cadence
throttle) is removed. Validator identities never cross data centers. The goal
is a quick cross-DC latency/throughput test.

## Configuration

The two DCs stay explicit in `.env` — the `BACKUP_*` lists ARE data center B
(do not flatten both sites into `VALIDATOR_IPS`):

```bash
SSH_USER=ubuntu
SSH_KEY_PATH=/path/to/fleet-key

# --- Data center A ---
VALIDATOR_IPS=A1,A2        # 2 live validators in DC A
SPARE_IPS=
RPC_IPS=A5,A6              # 2 pinned RPC trackers in DC A

# --- Data center B ---
BACKUP_VALIDATOR_IPS=B1,B2 # 2 live validators in DC B (active, not standby)
BACKUP_SPARE_IPS=
BACKUP_RPC_IPS=B5,B6       # 2 pinned RPC trackers in DC B
```

Any fleet box NOT listed must have `avalanchego` stopped (`pkill avalanchego`
on the box) so no stray process keeps a duplicate staking identity alive.

The node-to-DC mapping is visible everywhere: machines are named `m*` (DC A)
and `b*` (DC B), `reconcile endpoints` prints `name / site / role / host /
port` per node, `status.sh` shows the same names, and the Prometheus targets
carry a `site` label.

## Key layout

Registered validator keys span BOTH sites: `staking/l1/6,7` are DC A's
validators, `staking/l1/8,9` DC B's — all four are registered on the P-chain by
`02_create_l1.sh` (it registers `VALIDATOR_IPS` + `BACKUP_VALIDATOR_IPS`).
Every slot also has a non-validating home identity (`staking/l1/10..17`) worn
by the RPC trackers and by any cordoned validator slot. All needed keys are
already committed.

## Consensus parameters (`subnet-config.json`)

`k=20`, `alphaPreference=11`, `alphaConfidence=13`, `beta=12`,
`concurrentRepolls=4`, sized for the 4-validator set:

- k is the per-poll stake SAMPLE size (with repetition), not the validator
  count; oversampling dedups to at most one query per node on the wire.
- k=4 cannot work: `alphaConfidence=4` needs 100% connected stake, so the
  engine drops all queries the moment any validator is down (queries are
  dropped whenever connected stake < alphaConfidence/k), while
  `alphaConfidence=3` forces `alphaPreference == alphaConfidence` — the
  equal-alpha fork bug.
- 13/20 keeps polling and health with one of four validators down
  (0.75 connected > 13/20 = 0.65).
- `beta=12` cuts one-down finality bursts to a few seconds versus ~10s at
  `beta=25`; `concurrentRepolls=4` (must be <= beta) keeps repoll pipelining.

## Deploy (from the control host)

```bash
./01_bootstrap_primary_network.sh   # 5 local P-chain validators (leave running)
./02_create_l1.sh                   # register all 4 validators, write network.env
./03_wipe_and_deploy_l1.sh          # wipe + start all 8 nodes (prints the DC map)
./04_monitoring.sh                  # Prometheus + Grafana
./scripts/failover/status.sh        # expect "validators serving: 4/4"
./05_benchmark.sh                   # bombard all 4 RPC trackers (both DCs)
```

## What was kept from the failover tooling

The reconcile engine (fresh deploy, cordon/uncordon, clean, status, verify,
endpoints) is unchanged — it is the deploy path. `down.sh`/`up.sh` still
cordon a machine; with no spares configured, a downed validator's key simply
stays uncovered in its own DC until `up.sh`.
