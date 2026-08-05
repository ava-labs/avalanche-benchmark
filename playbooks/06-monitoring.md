# Playbook 06: monitoring

This playbook connects Prometheus and Grafana to the fleet. The dashboards
answer two questions. Does the chain produce blocks? Which machines carry
the stake?

## Prometheus

Make one scrape job that reads `/ext/metrics` from every node. Give each
target these labels:

```yaml
- targets: ["10.0.0.11:9650"]
  labels: { node: "a", role: "validator", dc: "A" }
```

The `node` label is the identity letter. The `dc` label is the same tag
as in `nodes.ini`.

Add one job for the app services. The settlement-feed exporter listens on
port 9701. Add one job for the weight exporter:

```bash
python3 monitoring/fleet-weight-exporter.py   # port 9091
```

The weight exporter reads the placement and the deployment records on the
control machine. It does not use a chain API. It therefore operates on an
isolated fleet.

## Grafana

Provision `monitoring/grafana-datasources.yml`. Provision every dashboard
from `monitoring/dashboards/`. Also provision each app's `dashboards/`
directory. For the settlement-feed app, this is the Direct Price Feed
dashboard. The app dashboard files have name prefixes. Put them in the
same provisioning directory as the base dashboards.

The failover dashboard shows the up/down state and the weight per machine,
for each data center. It also shows the height per node, the polls per
node, and the chain TPS.

Measure throughput from the chain-TPS panel:
`max(rate(avalanche_subnetevm_vm_eth_chain_txs_accepted[1m]))`. This value
comes from the accepted transactions on the chain.

## Health probe

Automation can call `fleet status` directly. The command exits with a
code that is not 0 only for real problems. These problems are: identity
drift, an `up` node with a silent API, and a required P-chain failure. An
isolated frozen fleet in its normal state exits with code 0.
