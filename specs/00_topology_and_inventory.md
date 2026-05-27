# Topology And Inventory Design Notes

This spec owns the machine roles, `.env` inventory, and artifact-placement model shared by all runtime steps.

## Core Model

The operator machine runs the scripts.

The scripts use SSH directly:

- to the benchmark host for P-Chain and benchmark commands;
- to node hosts for L1 node start/stop;
- locally when `BENCHMARK_HOST_IP=127.0.0.1`.

There is no second orchestration layer running on the benchmark host. The benchmark host is just another SSH target, plus the machine where latency-sensitive benchmark clients run.

## Terms

- **Operator machine**: the machine where the user runs `make`, `scripts/*`, and `create-l1`.
- **Benchmark host**: the machine identified by `BENCHMARK_HOST_IP`. It runs two P-Chain AvalancheGo processes and later `bombard`.
- **Node host**: a machine that runs one L1 AvalancheGo process.
- **L1 node identity**: a committed staking identity under `staking/l1/<n>`.

In AWS development, the operator machine can be outside the region. That is fine because measured load still comes from `bombard` on the benchmark host.

In client/manual mode, the operator machine may also be the benchmark host. Set `BENCHMARK_HOST_IP=127.0.0.1` to make benchmark-host commands run locally.

## Inventory Contract

The runtime requires a user-editable `.env` file:

```sh
SSH_USER=ubuntu
SSH_KEY=/path/to/private-key
BENCHMARK_HOST_IP=<benchmark host ip or 127.0.0.1>
DC1_NODE_IPS=<dc1 node ip 1>,<dc1 node ip 2>,...
DC2_NODE_IPS=<dc2 node ip 1>,<dc2 node ip 2>,...
L1_VALIDATOR_COUNT=5
```

`BENCHMARK_HOST_IP` must not be included in `DC1_NODE_IPS` or `DC2_NODE_IPS`.

`DC1_NODE_IPS` and `DC2_NODE_IPS` contain only L1 node hosts.

`L1_VALIDATOR_COUNT` controls L1 conversion only. It does not decide how many node hosts start AvalancheGo.

## Terraform Contract

Terraform is only a development convenience. The client target may provide pre-provisioned machines and a manually filled `.env`.

AWS single-DC development should create:

```text
1 benchmark host
6 DC1 node hosts
0 DC2 node hosts
```

Terraform output must write:

```sh
BENCHMARK_HOST_IP=<benchmark host>
DC1_NODE_IPS=<six node hosts, excluding benchmark host>
DC2_NODE_IPS=
L1_VALIDATOR_COUNT=5
```

The benchmark host and node hosts may use the same instance type and disk layout, but they are different roles.

## Runtime Directory

Use the same remote working directory everywhere:

```text
/data/avalanche-benchmark
```

Runtime state lives under:

```text
/data/avalanche-benchmark/runtime-data
```

## Artifact Placement

The runtime package is unpacked on the operator machine.

The package should include:

```text
create-l1
avalanchego
bombard
srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
config/genesis.json
config/chain-config.json
config/node-config.json
staking/pchain/1..2
staking/l1/1..15
staking/node-ids.env
scripts/*.sh
.env.example
```

`create-l1` stays on the operator machine. Do not ship it to node hosts or require it on the benchmark host.

Benchmark host receives:

```text
avalanchego
bombard
srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
```

Node hosts receive:

```text
avalanchego
srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
```

Chain-specific files and staking keys are copied during the relevant start step, not blindly by the first artifact-copy step.

`00_copy-artifacts.sh` stops existing `avalanchego` processes before copying binaries. This avoids `scp` failing when it tries to overwrite a running executable.

## L1 Node Placement

For current single-DC startup, L1 nodes run on every IP in `DC1_NODE_IPS`.

Node identity assignment is by node index when a committed identity exists:

```text
DC1_NODE_IPS[0] -> staking/l1/1
DC1_NODE_IPS[1] -> staking/l1/2
DC1_NODE_IPS[2] -> staking/l1/3
DC1_NODE_IPS[3] -> staking/l1/4
DC1_NODE_IPS[4] -> staking/l1/5
...
```

Do not label a process as validator or follower in the startup script. Validation status is decided by the P-Chain conversion validator set, not by `03_start-l1.sh`.

Current committed pool: `staking/l1/1..15`.
