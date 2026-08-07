# Playbook 01: provision a fleet

This playbook makes an L1 that produces blocks on your machines. The fleet
operates in isolation from the public network.

## Before you start

Make sure that these conditions are true:

- The control machine has ssh access to every fleet machine.
- Each machine has a fast disk. NVMe is good. A disk with fsync latency
  above one millisecond is not good for benchmark work.
- The file `.env` exists. Copy it from `.env.example`. Set the network, the
  P-chain API, the funded key, and the ssh values. Root is not necessary:
  the install runs under the ssh user by default. See
  [02-rootless-install.md](02-rootless-install.md) for the install options.
- The file `nodes.ini` exists. Copy a shape from `examples/` and set your
  hosts. Do not change the node numbers or the roles.
- The binaries exist in `bin/`. Run `make package-build` to build them.

## Procedure

```bash
./bin/l1 keygen                    # step 1: make the node identities
./bin/l1 create                    # step 2: register the L1 on the public network
./bin/fleet deploy follow          # step 3: start the P-chain node
./bin/fleet status                 # step 4: wait for READY TO FREEZE = yes
./bin/fleet pchain archive         # step 5: write ./pchain.tar.gz
./bin/fleet deploy frozen --dry-run  # step 6: test every host, change nothing
./bin/fleet deploy frozen          # step 7: deploy the full fleet
./bin/fleet status                 # step 8: confirm the result
```

In step 4, repeat `fleet status` until the P-chain MODE is `synced` and
READY TO FREEZE is `yes`.

Step 7 examines every host before it stops any node. If one host has a
problem, the command stops with a report and changes nothing. Correct the
host and run the command again.

## Result

`fleet status` shows every node `up` and exits with code 0. The heights
increase. The P-chain MODE is `frozen`. The L1 STATE is `complete`.

The next playbooks are [04-load-test.md](04-load-test.md),
[05-failover-drill.md](05-failover-drill.md), and
[03-monitoring.md](03-monitoring.md).
