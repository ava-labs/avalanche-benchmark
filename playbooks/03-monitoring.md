# Playbook 03: monitoring

This playbook starts Prometheus and Grafana on the control machine. Three
commands, no hand-written scrape configs. The dashboards answer three
questions. Does each chain produce blocks? Is every node up? Which
machines carry the stake?

## Start the stack

Run from the deployment root. Docker with the compose plugin must be
installed.

```bash
./bin/fleet targets > monitoring/targets.json
docker compose -f monitoring/docker-compose.yml up -d
```

Open Grafana at `http://<control-host>:3000`. The first login is
admin/admin. Start with the Fleet Health dashboard.

`fleet targets` renders the Prometheus scrape targets from `nodes.ini`
and the deployment records. Every target carries these labels:

- `node`: the inventory node number.
- `role`: the inventory role.
- `dc`: the data center tag, when the inventory sets one.
- `l1`: the name of the chain the node serves. The P-chain node has no
  `l1` label because it serves every chain.
- `l1_chain_id`: the blockchain ID of that chain, after `l1 create`.

Re-run `fleet targets` after every inventory change and after `l1 create`.
Prometheus reloads the file by itself.

## Per-chain dashboards

Every fleet dashboard has a chain dropdown. Pick a chain and every panel
shows only that chain. The dropdown lists the `l1` label values, so a new
chain in `nodes.ini` appears there without dashboard changes.

The dashboards, in the order to open them:

1. **Fleet Health**: the default view. Nodes up, P-chain beacon, height
   per node, throughput, poll success, stake weight per data center.
2. **Failover**: up/down and weight per machine and per data center. Use
   it during failover drills (playbook 05).
3. **Avalanche**: consensus internals per node. Open it when Fleet Health
   shows a problem and you need the cause.
4. **Machine**: CPU, memory, and disk per node process.

App dashboards provision from each app's `dashboards/` directory. The
compose file mounts `apps/settlement-feed/dashboards`; add one mount line
per additional app.

## Alerts

Prometheus evaluates the rules in `monitoring/alerts.yml`. Apps ship their
own rules next to their dashboards; the compose file mounts
`apps/settlement-feed/alerts.yml` the same way. Firing alerts appear at
`http://<control-host>:9090/alerts` and on `/api/v1/alerts`.

The rules are the trigger source for your operations automation. The kit
does not send notifications. To page or to automate a response, point an
Alertmanager at Prometheus or poll the API.

The severities:

- `critical`: act now. `NodeDown` is the failover trigger; confirm the
  machine is down and fence it before any identity move (playbook 06).
  `ValidatorWeightBenched` and `DiskSpaceCritical` also carry it.
- `warning`: investigate the same day. A node behind its peers, low poll
  success, block verify errors, low disk, CPU throttling.
- `info`: an audit signal. `RegisteredWeightChanged` fires on every stake
  move; verify it matches a planned swap or failover.

There is no height-stall rule on purpose. The EVM produces blocks only
when transactions arrive, so a quiet chain looks identical to a stalled
one. `NodeBehindPeers` catches the harmful case.

## The weight exporter

The compose stack runs `monitoring/fleet-weight-exporter.py` as a
service. It reads `deployment/placement.json` and
`deployment/public.json` on every scrape and serves `fleet_actual_weight`
per machine, with `dc` and `l1` labels. It uses no chain API, so it also
operates on an isolated frozen fleet. A key swap from `fleet place` shows
up on the next scrape.

## Measure throughput

Read the throughput panel on Fleet Health, or query directly:
`max(rate(avalanche_subnetevm_vm_eth_chain_txs_accepted{chain="<blockchain-id>"}[1m]))`.
The value comes from the accepted transactions on that chain.

## Health probe

Automation can call `fleet status` directly. The command exits with a
code that is not 0 only for real problems. These problems are: identity
drift, an `up` node with a silent API, and a required P-chain failure. An
isolated frozen fleet in its normal state exits with code 0.

## Without Docker

Run Prometheus and Grafana any other way with the same files:
`monitoring/prometheus.yml` (edit the weight-exporter and app targets to
`localhost`), `monitoring/targets.json`, the provisioning files
`monitoring/grafana-datasources.yml` (change the URL to
`http://localhost:9090`) and `monitoring/grafana-dashboards.yml`, and the
dashboards under `monitoring/dashboards/`. Start the weight exporter by
hand: `python3 monitoring/fleet-weight-exporter.py deployment 9091`.
