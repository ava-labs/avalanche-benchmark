# Create L1 Design Notes

This spec captures the current decisions for the second `benchctl` command:

```sh
./benchctl create-l1
```

The goal is to create one Subnet-EVM L1 on the already-running local P-Chain and convert it to an L1 using committed benchmark validator identities. The command runs on the **benchmark host**.

## Re-Entry Notes For Future Agents

`benchctl create-l1` and `scripts/02_create-l1.sh` are implemented. The command is intentionally not smoke-tested against a live P-Chain during normal verification because it creates a real new L1 each successful run.

The current Go module imports AvalancheGo wallet/platform packages and therefore uses Go `1.24.9`, matching the old `remote-failover` module requirement.

## Step Goals

After reading this file, an agent should be able to implement:

```sh
./benchctl create-l1
```

The command must:

- run from the runtime working directory on the benchmark host;
- require `.env`;
- require `L1_VALIDATOR_COUNT` in `.env`;
- fail immediately if `L1_VALIDATOR_COUNT` is missing, empty, not an integer, less than 1, or greater than 5;
- read the Subnet-EVM genesis from `./config/genesis.json`;
- talk to the local P-Chain RPC at `http://127.0.0.1:9650`;
- use the local-network EWOQ keychain to fund P-Chain transactions;
- issue `CreateSubnetTx`;
- re-sync the P-Chain wallet for the new subnet;
- issue `CreateChainTx` with the committed Subnet-EVM VM ID;
- create the conversion validator set from the first `L1_VALIDATOR_COUNT` committed L1 validator identities;
- compute BLS proofs of possession locally from committed `staking/l1/<n>/signer.key` material embedded in `benchctl`;
- issue `ConvertSubnetToL1Tx`;
- write `./runtime-data/l1.env`, overwriting any previous file;
- print concise success output with subnet ID, chain ID, and validator count.

`create-l1` intentionally creates a new L1 every time it runs. One P-Chain can host many experiment L1s while the operator edits `config/genesis.json` between runs. The latest run wins for downstream scripts through `runtime-data/l1.env`.

## Non-Goals For This Step

Do not implement these in this step:

- starting remote L1 validator nodes;
- starting RPC nodes;
- copying chain config to remote nodes;
- bombard;
- failover/key-swap;
- runtime flags or CLI overrides;
- L1 validator set mutation after conversion.

## Runtime Output

Write this file on every successful run:

```text
./runtime-data/l1.env
```

Required contents:

```sh
L1_SUBNET_ID=<subnet id>
L1_CHAIN_ID=<chain id>
L1_VALIDATOR_COUNT=<count used for conversion>
```

Overwrite the file each time `create-l1` succeeds.

Do not append L1 output values to `.env`. `.env` is operator-supplied inventory/config; `runtime-data/l1.env` is generated run output.

## Decided Q&A

### Q1. Should `create-l1` be idempotent if `runtime-data/l1.env` already exists?

No.

The operator may start one P-Chain and create many L1s while experimenting with `config/genesis.json`. Each call creates a fresh subnet, chain, and conversion transaction. The command overwrites `runtime-data/l1.env` with the latest IDs.

### Q2. How many validators should be registered during conversion?

Read required env var:

```text
L1_VALIDATOR_COUNT
```

Use the first N committed L1 identities:

```text
staking/l1/1
staking/l1/2
staking/l1/3
staking/l1/4
staking/l1/5
```

No flags. Missing or invalid `L1_VALIDATOR_COUNT` is a hard error.

### Q3. Should validator PoPs come from live nodes?

No.

Precompute the conversion validator PoPs locally from committed L1 signer keys. This is better logistically because L1 validator nodes do not need to be online before conversion.

### Q4. What validator weights and balances should be used?

Use equal weights and equal balances for every registered L1 validator.

The old `remote-failover` values are acceptable:

```go
Weight:  units.Schmeckle
Balance: units.Avax
```

The specific values are not important for current benchmarks as long as all validators are equal.

### Q5. What validator manager address should conversion use?

Use an empty validator-manager address:

```go
[]byte{}
```

This benchmark does not manage the chain validator set after conversion.

### Q6. Which wrapper script should call this command?

Add:

```sh
./scripts/02_create-l1.sh
```

It should use the same local-or-SSH helper as `01_start-pchain.sh`:

- if `BENCHMARK_HOST_IP` is `127.0.0.1`, `localhost`, or `::1`, run locally from the runtime working directory;
- otherwise SSH to the benchmark host and run in `/data/avalanche-benchmark`.

### Q7. Should `create-l1` use `config/chain-config.json`?

No.

`config/chain-config.json` is for later L1 node startup, where it must be placed under the chain-specific config path after the chain ID is known.

### Q8. Should committed L1 key files be shipped separately?

No.

Like P-Chain keys, the L1 validator key material needed by `benchctl` should be embedded into the binary. Later node-start commands can write the selected L1 staking files into each validator node's default staking directory.

## Implementation Reference

Closest old references:

- `~/avalanche-benchmark-bombard-ws-worker-pool/local/internal/network/network.go`
- `~/avalanche-benchmark-bombard-ws-worker-pool/remote-failover/cmd/failover/create_l1.go`

Use the old `remote-failover` conversion model: validators are known from committed keys and do not need to be running when `ConvertSubnetToL1Tx` is issued.
