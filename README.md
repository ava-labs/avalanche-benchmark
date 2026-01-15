# Avalanche Benchmark CLI

Benchmark Avalanche L1 (subnet-evm) throughput. Creates isolated networks, floods transactions, displays real-time metrics.

## Quick Start

```bash
# Download latest release
wget https://github.com/ava-labs/avalanche-benchmark/releases/latest/download/avalanche-benchmark-linux-amd64.tar.gz
tar -xzf avalanche-benchmark-linux-amd64.tar.gz
cd avalanche-benchmark-linux-amd64

# Setup dependencies
bash scripts/setup-all.sh
source ~/.benchmark-env

# Run
./benchmark start
./benchmark flood
./benchmark monitor
./benchmark shutdown
```

## Installation

Download from [Releases](https://github.com/ava-labs/avalanche-benchmark/releases):

| Platform | Download |
|----------|----------|
| Linux amd64 | `avalanche-benchmark-linux-amd64.tar.gz` |
| Linux arm64 | `avalanche-benchmark-linux-arm64.tar.gz` |
| macOS amd64 | `avalanche-benchmark-darwin-amd64.tar.gz` |
| macOS arm64 | `avalanche-benchmark-darwin-arm64.tar.gz` |

Each release includes:
- `benchmark` - main CLI
- `bombard` - transaction flooder
- `staking/` - pre-generated staking keys
- `scripts/` - setup scripts

### Setup Dependencies

```bash
# Downloads avalanchego + subnet-evm plugin
bash scripts/setup-all.sh
source ~/.benchmark-env
```

For offline servers, pre-download [avalanchego](https://github.com/ava-labs/avalanchego/releases) and [subnet-evm](https://github.com/ava-labs/subnet-evm/releases).

## Commands

| Command | Description |
|---------|-------------|
| `start` | Start network + create L1 |
| `flood` | Start flooding transactions |
| `stop-flood` | Stop flooding |
| `monitor` | Show live TPS metrics |
| `shutdown` | Stop everything |
| `status` | Show node health |
| `logs` | View node logs |

## Configuration

### Config File

```bash
./benchmark start --config config.json
```

```json
{
  "primaryNodeCount": 2,
  "l1ValidatorNodeCount": 5,
  "l1RpcNodeCount": 2,
  "nodeFlags": {
    "log-level": "warn"
  }
}
```

| Field | Default | Description |
|-------|---------|-------------|
| `primaryNodeCount` | 2 | Primary network nodes (min: 2) |
| `l1ValidatorNodeCount` | 2 | L1 validator nodes (consensus) |
| `l1RpcNodeCount` | 1 | L1 RPC nodes (load balancing, no consensus) |
| `nodeFlags` | `{}` | Flags passed to avalanchego |

### Custom Genesis

```bash
./benchmark start --genesis custom-genesis.json
```

Default genesis is optimized for max throughput:

```json
{
  "config": {
    "chainId": 99999,
    "feeConfig": {
      "gasLimit": 500000000,
      "targetBlockRate": 1,
      "minBaseFee": 1,
      "targetGas": 18446744073709551615,
      "baseFeeChangeDenominator": 9223372036854775807,
      "minBlockGasCost": 0,
      "maxBlockGasCost": 0,
      "blockGasCostStep": 0
    }
  },
  "alloc": {
    "8db97C7cEcE249c2b98bDC0226Cc4C2A57BF52FC": {
      "balance": "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"
    }
  }
}
```

| Setting | Value | Purpose |
|---------|-------|---------|
| `gasLimit` | 500M | Max gas per block (~23,800 transfers) |
| `targetBlockRate` | 1 | 1-second blocks |
| `minBaseFee` | 1 wei | Lowest possible fee |
| `targetGas` | MaxUint64 | Prevents fee increases |
| `baseFeeChangeDenominator` | MaxInt64 | Keeps fees frozen |
| `minBlockGasCost` | 0 | No block production overhead |

### Custom Chain Config

```bash
./benchmark start --chain-config custom-chain.json
```

Default chain config is optimized for maximum throughput. Key settings:

```json
{
  "database-type": "pebbledb",
  "min-delay-target": 200000000,

  "trie-clean-cache": 102400,
  "trie-dirty-cache": 102400,
  "snapshot-cache": 51200,

  "tx-pool-price-limit": 1,
  "tx-pool-global-slots": 262144,
  "tx-pool-global-queue": 131072,

  "push-gossip-frequency": "50ms",
  "skip-tx-indexing": true,
  "local-txs-enabled": true,
  "allow-unfinalized-queries": true
}
```

| Setting | Purpose |
|---------|---------|
| `min-delay-target` | Block delay in nanoseconds (200ms) |
| `trie-clean-cache` | Clean state cache in MB |
| `trie-dirty-cache` | Dirty state cache in MB |
| `tx-pool-global-slots` | Max pending transactions |
| `push-gossip-frequency` | TX propagation interval |
| `skip-tx-indexing` | Disable indexing for speed |

## CLI Options

```bash
# Network topology
./benchmark start --primary-nodes 2 --l1-validators 5 --l1-rpcs 2

# Custom configs
./benchmark start --genesis genesis.json --chain-config chain.json --config config.json

# Flood tuning
./benchmark flood --keys 600 --batch 50
```

## Performance

```
Max TPS: ~16,000 (limited by 1.8MB block size)
Gas limit: 500M per block
Block time: 1 second
```

## Environment Variables

| Variable | Description |
|----------|-------------|
| `AVALANCHEGO_PATH` | Path to avalanchego binary |
| `AVALANCHEGO_PLUGIN_DIR` | Path to plugins directory |
| `EVMBOMBARD_PATH` | Path to bombard binary |

## Building from Source

```bash
git clone https://github.com/ava-labs/avalanche-benchmark.git
cd avalanche-benchmark
make build
./bin/benchmark start
```

## Troubleshooting

| Error | Fix |
|-------|-----|
| `avalanchego not found` | Run `scripts/setup-all.sh` or set `AVALANCHEGO_PATH` |
| `bombard not found` | Set `EVMBOMBARD_PATH` |
| Ports in use | `./benchmark shutdown` or reboot |
| Low TPS | Increase `--keys`, check RAM/config |
