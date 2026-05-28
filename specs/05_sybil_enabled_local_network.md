# Sybil Enabled Local Network Design Notes

This spec captures the target design for running the benchmark with real validator semantics:

```text
--sybil-protection-enabled=true
```

Equivalently, omit `--sybil-protection-enabled` because AvalancheGo defaults it to `true`.

## Why This Exists

The existing scripts use `--sybil-protection-enabled=false`. That mode is useful for quick local testing, but it changes validator semantics:

- connected peers get synthetic validator weight;
- subnet validator-manager calls can be redirected through the Primary Network validator set;
- nodes can create/serve subnet chains without the normal `--track-subnets` filter.

This made old benchmark results misleading. For production-closer benchmarks, use sybil protection enabled.

## Source Facts

AvalancheGo loads Primary Network genesis in this order:

1. `--genesis-file-content`
2. `--genesis-file`
3. built-in genesis for the given `--network-id`

Source: `config/config.go:getGenesisData`.

AvalancheGo rejects custom genesis files for standard networks:

```text
Mainnet, Fuji/Testnet, Local
```

Source: `genesis/genesis.go:FromFile` and `FromFlag`.

Therefore:

- `--network-id=local` is a local network, but it uses the embedded local genesis.
- `--network-id=local --genesis-file=...` is not valid.
- A custom Primary Network genesis requires a non-standard numeric network ID, for example `12346`.

For unknown/non-standard network IDs, AvalancheGo starts from the local config shape and replaces the network ID.

Source: `genesis/config.go:GetConfig`.

## Two Valid Modes

### Mode A: Built-In Local Genesis With Disjoint L1 Identities

Use this for the simplest sybil-enabled benchmark with the production-like constraint that Primary Network validator identities and L1 validator identities are different.

AvalancheGo built-in local genesis has exactly five initial Primary Network stakers. Their NodeIDs match committed `staking/l1/1..5`:

```text
L1_1_NODE_ID=NodeID-7Xhw2mDxuDS44j42TCB6U5579esbSt3Lg
L1_2_NODE_ID=NodeID-MFrZFVCXPv5iCn6M9K6XduxGTYp891xXZ
L1_3_NODE_ID=NodeID-NFBbbJ4qCmNaCzeW7sxErhvWqvEQMnYcN
L1_4_NODE_ID=NodeID-GWPcbFJZFfZreETSoWjPimr846mXEKCtu
L1_5_NODE_ID=NodeID-P7oB2McjBGgW2NXXWVYjV8JEDFoW9xDE5
```

These identities must be used only for the Primary Network processes in this benchmark mode.

Use a disjoint identity range for the L1 validators. For five L1 validators, use committed `staking/l1/6..10`:

```text
L1_6_NODE_ID=NodeID-9yhg1FqY9Nf71PiB8Ep5tZz78QMWG5moy
L1_7_NODE_ID=NodeID-45wqqh1yKJvmowjZBmCcJtCY3DDM28Gsa
L1_8_NODE_ID=NodeID-B3NXXjgdprx9hT1WLm116LAWKCsqV2s8g
L1_9_NODE_ID=NodeID-Lp3RFM13PsA6AN7PhEFnCdH5AAxvDBkZr
L1_10_NODE_ID=NodeID-6VQY6t1oqKHhsjErGqUjU2qvGAtTyLVio
```

Reason:

- One AvalancheGo process has exactly one staking identity / NodeID.
- If the Primary Network validator identity and L1 validator identity must be different, they must be different processes.
- With sybil protection enabled, Primary Network nodes that do not pass `--track-subnets` will not instantiate the L1.

On five machines, run two AvalancheGo processes per machine:

```text
DC1_NODE_IPS[0] primary process -> staking/l1/1, HTTP 9650, staking 9651
DC1_NODE_IPS[1] primary process -> staking/l1/2, HTTP 9650, staking 9651
DC1_NODE_IPS[2] primary process -> staking/l1/3, HTTP 9650, staking 9651
DC1_NODE_IPS[3] primary process -> staking/l1/4, HTTP 9650, staking 9651
DC1_NODE_IPS[4] primary process -> staking/l1/5, HTTP 9650, staking 9651

DC1_NODE_IPS[0] L1 process -> staking/l1/6,  HTTP 9652, staking 9653
DC1_NODE_IPS[1] L1 process -> staking/l1/7,  HTTP 9652, staking 9653
DC1_NODE_IPS[2] L1 process -> staking/l1/8,  HTTP 9652, staking 9653
DC1_NODE_IPS[3] L1 process -> staking/l1/9,  HTTP 9652, staking 9653
DC1_NODE_IPS[4] L1 process -> staking/l1/10, HTTP 9652, staking 9653
```

Do not start the separate benchmark-host `pchain/1..2` processes for this mode. Those NodeIDs are not built-in local genesis stakers.

Primary process flags:

```text
--network-id=local
--http-host=0.0.0.0
--public-ip=<node-ip>
--http-port=9650
--staking-port=9651
--bootstrap-ips=<other primary staking ports>
--bootstrap-ids=<other primary NodeIDs>
```

Primary processes must not pass:

```text
--sybil-protection-enabled=false
--genesis-file
--track-subnets
```

After the Primary Network is healthy:

1. Run `create-l1` against node1 P-Chain RPC, not the benchmark host P-Chain RPC.
2. Convert the subnet to an L1 with `staking/l1/6..10`, not `staking/l1/1..5`.
3. Start or restart the five L1 processes with:

```text
--track-subnets=<L1_SUBNET_ID>
```

and with the L1 chain/subnet config installed under the L1 process data dir.

L1 process flags:

```text
--network-id=local
--http-host=0.0.0.0
--public-ip=<node-ip>
--http-port=9652
--staking-port=9653
--track-subnets=<L1_SUBNET_ID>
--bootstrap-ips=<primary staking ports plus other L1 staking ports>
--bootstrap-ids=<primary NodeIDs plus other L1 NodeIDs>
```

Do not pass `--sybil-protection-enabled=false` to either process.

Alternative machine layout:

- If ten machines are available, use five machines for Primary Network `staking/l1/1..5` and five different machines for L1 `staking/l1/6..10`.
- In that layout each machine can use default ports `9650/9651`, but the identity split is the same.

### Mode B: Custom Local/Private Network

Use this only when the Primary Network validator set must differ from built-in local genesis, for example:

- more than five Primary Network validators;
- different Primary Network NodeIDs;
- using `staking/pchain/1..2` as real Primary Network validators;
- custom genesis weights, start times, or staking allocations.

Required changes:

- Choose a non-standard network ID, for example:

```text
BENCHMARK_NETWORK_ID=12346
```

- Create a Primary Network genesis file, for example:

```text
config/primary-genesis.json
```

- Start every node with:

```text
--network-id=$BENCHMARK_NETWORK_ID
--genesis-file=<path-to-primary-genesis.json>
```

Every node must use the exact same genesis file from first boot. If the genesis file changes, wipe all node DBs before restarting.

Do not use `--network-id=local` with `--genesis-file`; AvalancheGo rejects that combination.

## Required Script Changes

The current scripts are built around a separate benchmark-host P-Chain pair and a one-process-per-node L1 start. Sybil-enabled Mode A should not use that topology.

Needed changes:

- Add a new sybil-enabled start path, or a separate script, that starts the first five `DC1_NODE_IPS` as the Primary Network genesis validators using `staking/l1/1..5`.
- Make `create-l1` configurable for the P-Chain RPC URL. It currently assumes:

```text
http://$BENCHMARK_HOST_IP:9650
```

For Mode A it should use:

```text
http://${DC1_NODE_IPS[0]}:9650
```

- Make `create-l1` configurable for the L1 validator identity range. It currently uses the first `L1_VALIDATOR_COUNT` identities. For this mode it must use `staking/l1/6..10`, for example via:

```text
L1_VALIDATOR_START_INDEX=6
L1_VALIDATOR_COUNT=5
```

- Add a sybil-enabled L1 start path that starts five L1 processes using `staking/l1/6..10`.
- On five colocated machines, the L1 process must use non-default ports, for example HTTP `9652` and staking `9653`.
- In sybil-enabled Mode A, remove benchmark-host P-Chain nodes from bootstrap lists. Bootstrap primary processes against primary staking ports, and L1 processes against primary staking ports plus other L1 staking ports.
- Make `04_bombard.sh` target the L1 process RPC port for this mode:

```text
http://<node-ip>:9652/ext/bc/<L1_CHAIN_ID>/rpc
```

If using ten machines with one process per machine, the L1 RPC port may remain `9650`.

## Verification

Do not trust a run until these checks pass.

Primary Network:

```text
info.getNodeID on primary process i matches staking/l1/<i>, for i=1..5
health.health is healthy on every primary process
info.peers shows the other Primary Network validators
```

L1:

```text
info.getNodeID on L1 process i matches staking/l1/<i+5>, for i=1..5
eth_chainId responds on every L1 validator RPC
info.peers on L1 processes shows peers advertising <L1_SUBNET_ID>
platform.getCurrentValidators(subnetID=<L1_SUBNET_ID>) returns 5 validators
```

Metrics:

```text
avalanche_stake_num_validators{chain="<L1_CHAIN_ID>"} == 5
avalanche_stake_percent_connected{chain="<L1_CHAIN_ID>"} >= healthy threshold
```

For default Snow parameters `k=20`, `alphaConfidence=15`, the healthy threshold is `0.8`.

The important failure signal is any synthetic-looking validator count such as `2` or `15` for a five-validator L1. That means the benchmark is not using the intended canonical L1 validator set.

Another failure signal is any overlap between Primary Network process NodeIDs and L1 validator process NodeIDs.

## Production Similarity

Sybil-enabled local mode is closer to production because:

- validator weight comes from P-Chain state, not synthetic peer connections;
- non-validator RPC nodes stay non-validators;
- `--track-subnets` is meaningful again;
- connected-stake health reflects the canonical L1 validator set.

It is still not production:

- it uses test keys;
- it uses local/private network genesis;
- economics and staking durations are local-test values;
- AWS topology and latency may not match production.

For benchmark comparisons, this is still the right baseline because it removes the synthetic validator behavior that distorted the old results.
