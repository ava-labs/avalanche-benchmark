# Playbook 04: validator swap (identity and weight failover)

Goal: move an active validator identity onto a spare machine, and back,
without the chain pausing. Two independent mechanisms; both work airgapped
at the fleet level.

## Key swap: `fleet place`

`fleet place <identity-letter> <node>` swaps the identity a machine runs
with whatever identity the target machine held. One move per call, roughly
20 to 25 seconds: converge placement, push keys, restart the two mismatched
machines. Placement on the control machine is the single source of truth;
deploy and start honour it, so a swap survives redeploys.

```bash
./bin/fleet stop 2          # lose the machine carrying heavy identity b
./bin/fleet place b 5       # b now runs on machine 5 (a spare site's box)
./bin/fleet status          # all heavy identities serving again
# restore canonical placement later:
./bin/fleet start 2
./bin/fleet place b 2
```

The chain keeps producing through the whole sequence; the drill's pass
criterion is height continuity on the dashboard.

## Weight move: `l1 set-weight`

`./bin/l1 set-weight <letter> <1|1000|100000>` changes stake weight through
the committee validators. It issues real P-chain transactions, so it needs
the funding key and an unfrozen, reachable P-chain: use it on the following
shape, not on an airgapped frozen fleet.

## Caveat

Grafana's per-node labels come from the Prometheus scrape config, which is
written at deploy time; after a `place`, a machine's identity letter changes
but its scrape label does not until the scrape config is regenerated. The
weight-per-machine panels (fed by the weight exporter) are live and correct
throughout.
