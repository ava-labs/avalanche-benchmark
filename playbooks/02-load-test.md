# Playbook 02: load test

This playbook puts transaction load on the fleet and measures the result
correctly.

## Procedure

```bash
./bin/bombard -rps 1000 -duration 10m
```

The load generator sends transactions to every `role=rpc` node.

Increase the rate in steps, for example 1000, then 2000, then 4000. Do not
go to the target rate in one step. The important measurement is the rate
where the p99 latency separates from the p50 latency. One large step hides
this point.

## How to measure

- Measure throughput from the block timestamps. The Grafana chain-TPS panel
  shows this value:
  `rate(avalanche_subnetevm_vm_eth_chain_txs_accepted[1m])`.
- Do not measure throughput from the bombard screen. Its value is measured
  at the observer. If the observer falls behind, the value becomes wrong.
- The bombard latency histogram is correct. If this latency grows and the
  block rate does not, the mempool has a backlog. A backlog means the
  offered load is more than the fleet can mine. This is a valid result,
  not an error.
- Do not compare measurement windows that contain a node restart.

## What limits throughput

Hardware is the first limit. A consensus poll round costs approximately 3
milliseconds on free CPU cores. The same round costs approximately 9.5
milliseconds on busy cores. Disk fsync latency sets the lower limit for
block times.

The consensus parameters are the second limit. Read docs/CONSENSUS-TUNING.md
before you change `k` or the alpha values. The values in
`chains/default/subnet-config.json` are safe. We selected them after we saw a
finalization fork with more aggressive values.
