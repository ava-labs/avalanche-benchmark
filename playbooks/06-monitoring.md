# Playbook 06: monitoring

Goal: Prometheus scraping every node and Grafana answering the two questions
that matter: is the chain producing, and who is carrying it.

## Prometheus

One scrape job over every node's `/ext/metrics`, labeled per target:

```yaml
- targets: ["10.0.0.11:9650"]
  labels: { node: "a", role: "validator", dc: "A" }
```

`node` is the identity letter, `dc` matches nodes.ini. Add the app's
services (the settlement-feed exporter on :9701) and the fleet weight
exporter as their own jobs:

```bash
python3 monitoring/fleet-weight-exporter.py   # :9091, reads deployment/ + nodes.ini
```

The weight exporter is airgap-safe: it reads control-side placement and
records, no chain API.

## Grafana

Provision `monitoring/grafana-datasources.yml` and every dashboard under
`monitoring/dashboards/`, plus each app's `dashboards/` overlay (for
settlement-feed: the Direct Price Feed board). App dashboards are
name-prefixed files; drop them in the same provisioning directory.

- Failover board: per-DC up/down state timelines and weight-per-machine,
  height and polls by node, chain TPS.
- Chain TPS panel is `max(rate(avalanche_subnetevm_vm_eth_chain_txs_accepted[1m]))`:
  block-timestamp truth, benchmark from it.

## Health probe

`fleet status` exits nonzero only on real problems (identity drift, an up
node whose API is silent, a required P-chain failure), including on an
airgapped frozen fleet, so it is safe to wire into automation directly.
