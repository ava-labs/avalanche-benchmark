# Playbook 02: load test

Goal: sustained transaction load with trustworthy numbers.

## Run

```bash
./bin/bombard -rps 1000 -duration 10m   # fans across every role=rpc node
```

Step the rate (1000, 2000, 4000) rather than jumping to the target: the
interesting number is where p99 detaches from p50, and a single big jump
hides it.

## Measure

- Chain truth is block timestamps. The Grafana chain-TPS panel
  (`rate(avalanche_subnetevm_vm_eth_chain_txs_accepted[1m])`) reports it
  durably. Do not benchmark from the bombard TUI: its mined-tps is
  observer-side and falls into a sawtooth when the watcher lags.
- Submit-to-mined latency from bombard's histogram is real; a growing gap
  between it and block cadence means the mempool is backing up (offered load
  exceeds what the fleet mines; that is a finding, not an error).
- Exclude windows spanning a restart when comparing configurations.

## What bounds throughput

Hardware first: consensus poll rounds cost ~3ms on dedicated cores against
~9.5ms on starved ones, and fsync latency sets the floor under block times
(NVMe ~0.03ms vs EBS ~3ms). Consensus parameters second: see
CONSENSUS-TUNING.md before touching k or the alphas; the safe operating
point shipped in `subnet-config.json` was chosen after observing a real
finalization fork under an aggressive one.
