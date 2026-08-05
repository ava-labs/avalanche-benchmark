# Playbook 03: failover drills

Goal: prove the chain survives what you fear, on purpose, under load.

Start load first ([02-load-test.md](02-load-test.md)): a failover drill
without load proves nothing about the thing you actually care about.

## Node loss

```bash
./bin/fleet stop 5              # graceful loss; data preserved
./bin/fleet destroy 5           # abrupt loss: SIGKILL + this L1's chain data
./bin/fleet start 5             # recover; idempotent, touches only broken nodes
```

The shipped weights mean the chain keeps producing through the loss of any
one heavy validator (67% of stake remains against a 63.3% quorum) and pauses,
by design, on two. Watch height continuity on the dashboard through the
drill: the pass criterion is that block production never stops.

## Site loss

Write it out as node numbers (`fleet stop 5 6 7 8 11 12`); there is
deliberately no dc= selector, because one command must not be able to take
half the fleet down by typo. Bring the site back with the same numbers on
`fleet start`.

## Identity failover between machines

[04-validator-swap.md](04-validator-swap.md) for moving a heavy identity to
a spare machine (key swap) and back, or shifting stake weight through the
committee.

## Machine reboot

`./bin/fleet pchain start` first (every other node bootstraps from it),
then `./bin/fleet start`. On rootless installs nothing auto-starts on boot;
on system installs systemd brings nodes back by itself.
