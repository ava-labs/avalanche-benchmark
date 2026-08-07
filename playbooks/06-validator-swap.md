# Playbook 06: validator swap

This playbook moves an active validator identity to a spare machine and
back. The chain does not stop during the move. There are two mechanisms.
They are independent.

## Mechanism 1: key swap with `fleet place`

`fleet place <identity-letter> <node>` moves one identity to one machine.
The identity that was on that machine moves back in exchange. One call
makes one move. One move takes approximately 20 to 25 seconds.

The placement file on the control machine is the single source of truth.
`deploy` and `start` obey it. A swap therefore survives a redeploy.

```bash
./bin/fleet stop 2          # the machine with heavy identity b is lost
./bin/fleet place b 5       # identity b now runs on spare machine 5
./bin/fleet status          # confirm: all heavy identities serve
```

To restore the initial placement:

```bash
./bin/fleet start 2
./bin/fleet place b 2
```

The pass condition is: the height panel shows no stop.

This mechanism does not need the public network. It operates on an
isolated frozen fleet.

## Mechanism 2: weight change with `l1 set-weight`

```bash
./bin/l1 set-weight <letter> <1|1000|100000>
```

This command sends real P-chain transactions through the committee
validators. It needs the funding key and a reachable, not frozen,
P-chain. Use it in the `follow` mode only. Do not use it on an isolated
frozen fleet.

## Monitoring after a place

The scrape targets label every node by its machine slot number, not by
its identity letter (see playbooks/03-monitoring.md). A `place` moves an
identity between machines; it changes no slot, host, or port. The scrape
labels therefore stay correct. The identity movement itself shows in the
weight exporter's `identity` label on the weight-per-machine panels.
