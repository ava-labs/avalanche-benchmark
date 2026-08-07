# Playbook 05: failover drills

This playbook removes nodes on purpose and shows that the chain continues.

Start load before each drill. See [04-load-test.md](04-load-test.md). A
drill without load does not show the behavior you care about.

## Drill: lose one node

```bash
./bin/fleet stop 5              # controlled loss; the data stays
./bin/fleet destroy 5           # sudden loss: SIGKILL, then delete this L1's chain data
./bin/fleet start 5             # recover the node
```

`fleet start` is safe to repeat. It only restarts nodes that are down,
that run the wrong identity, or that do not answer.

The chain continues through the loss of one heavy validator. The two
other heavy validators hold 67% of the stake, and the quorum is 63.3%.
The chain stops when two heavy validators are down. This stop is the
designed behavior.

Watch the height panel on the dashboard during the drill. The pass
condition is: block production does not stop.

## Drill: lose one site

Write the site as node numbers:

```bash
./bin/fleet stop 5 6 7 8 11 12
```

There is no `dc=` selector. This is intentional. One short command must
not be able to stop half of the fleet. Recover the site with the same
numbers on `fleet start`.

## Drill: move a validator identity

See [06-validator-swap.md](06-validator-swap.md). That playbook moves a
heavy identity to a spare machine and back.

## Drill: machine reboot

After a reboot, start the P-chain node first. Every other node
bootstraps from it.

```bash
./bin/fleet pchain start
./bin/fleet start
```

On a rootless install, no node starts at boot. On a system install,
systemd starts the nodes.
