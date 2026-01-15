# Avalanche Benchmark CLI

Benchmark Avalanche L1 (subnet-evm) throughput. Creates isolated networks, floods transactions, displays real-time metrics.

## Quick Start

```bash
make build
./bin/benchmark start
./bin/benchmark flood
./bin/benchmark monitor
./bin/benchmark shutdown
```

## Deployment

Zero build dependencies on target - just copy binaries and run.

```bash
# Build for Linux
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o benchmark-linux ./cmd/benchmark
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bombard-linux ./cmd/bombard

# Copy to server
scp benchmark-linux bombard-linux ubuntu@server:~/
scp -r staking scripts ubuntu@server:~/

# On server
bash ~/scripts/setup-all.sh   # Downloads avalanchego + subnet-evm
source ~/.benchmark-env
./benchmark start --l1-validators 5
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
benchmark start --config config.json
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
benchmark start --genesis custom-genesis.json
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
benchmark start --chain-config custom-chain.json
```

Default chain config auto-scales based on system RAM. Key settings:

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

### RAM-Based Auto-Scaling

Chain config caches auto-scale based on detected RAM:

| System RAM | trie-clean | trie-dirty | tx-pool-slots |
|------------|------------|------------|---------------|
| < 32 GB | 4.5 GB | 4.5 GB | 32K |
| 32-64 GB | 9 GB | 9 GB | 64K |
| 64-128 GB | 18 GB | 18 GB | 128K |
| 128-256 GB | 36 GB | 36 GB | 256K |
| 256-512 GB | 72 GB | 72 GB | 512K |
| 512+ GB | 144 GB | 144 GB | 1M |

## CLI Options

```bash
# Network topology
benchmark start --primary-nodes 2 --l1-validators 5 --l1-rpcs 2

# Custom configs
benchmark start --genesis genesis.json --chain-config chain.json --config config.json

# Flood tuning
benchmark flood --keys 600 --batch 50
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

## Troubleshooting

| Error | Fix |
|-------|-----|
| `avalanchego not found` | Run `scripts/setup-all.sh` or set `AVALANCHEGO_PATH` |
| `bombard not found` | Set `EVMBOMBARD_PATH` |
| Ports in use | `benchmark shutdown` or reboot |
| Low TPS | Increase `--keys`, check RAM/config |

## License

Apache 2.0
