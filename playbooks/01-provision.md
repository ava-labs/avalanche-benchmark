# Playbook 01: provision a fleet

Goal: an L1 producing blocks on your machines, airgapped from the public
network, in one sitting.

## Prerequisites

- Machines reachable over ssh from the control machine (see `examples/` for
  inventory shapes), NVMe or sub-millisecond-fsync disks for benchmark work.
- `.env` from `.env.example`: network, P-chain API, funded key, ssh access.
  For hosts where you have no root, also set `REMOTE_DIR`
  (see [05-rootless-install.md](05-rootless-install.md)).
- `nodes.ini` from an example shape. Numbers and roles are load-bearing;
  hosts are yours to change.
- Binaries: `make package-build` (builds avalanchego pinned to the kit's
  commit plus the kit binaries into `bin/`).

## Steps

```bash
./bin/l1 keygen                    # identities for every inventory node
./bin/l1 create                    # register the L1 on the public network
./bin/fleet deploy follow          # P-chain node follows the public network
./bin/fleet status                 # wait: synced, both validator sets, READY TO FREEZE yes
./bin/fleet pchain archive         # snapshot ./pchain.tar.gz
./bin/fleet deploy frozen --dry-run  # preflight every host, mutate nothing
./bin/fleet deploy frozen          # the airgapped fleet, P-chain first
./bin/fleet status                 # every node up, heights advancing
```

`deploy frozen` validates every host before stopping anything; a host
mismatch (missing tool, unwritable path, full disk) aborts with every
problem listed and the fleet untouched.

## Verification

`fleet status` exits 0, every node `up`, heights advancing, P-chain MODE
`frozen`, L1 STATE `complete`. From here: load ([02](02-load-test.md)),
drills ([03](03-failover-drill.md)), dashboards ([06](06-monitoring.md)).
