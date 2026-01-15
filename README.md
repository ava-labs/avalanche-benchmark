# Avalanche Benchmark CLI

A CLI tool for benchmarking Avalanche L1 (subnet-evm) throughput. Creates an isolated local network, floods transactions, and displays real-time metrics.

## Quick Start

```bash
make build
./bin/benchmark start
./bin/benchmark flood
./bin/benchmark monitor
./bin/benchmark shutdown
```

## Prerequisites

| Binary | Description | Location |
|--------|-------------|----------|
| `avalanchego` | Avalanche node | `AVALANCHEGO_PATH` or `./bin/avalanchego` |
| `subnet-evm` | EVM plugin | `AVALANCHEGO_PLUGIN_DIR` or `./plugins/srEXi...` |

The `bombard` flooder is built automatically with `make build`.

## Commands

| Command | Description |
|---------|-------------|
| `start` | Start network + create L1 |
| `flood` | Start flooding transactions |
| `stop-flood` | Stop flooding |
| `monitor` | Show live TPS metrics |
| `shutdown` | Stop everything |
| `status` | Show node health and endpoints |
| `logs` | View node logs (`logs validator 1 -f`) |
| `flood-status` | Show flooding process status |

### Start Options

```bash
benchmark start [flags]
  --primary-nodes N     Primary network validators (default: 2, min: 2)
  --l1-validators N     L1 validator nodes (default: 2)
  --l1-rpcs N           L1 RPC-only nodes (default: 1)
  --genesis PATH        Custom subnet-evm genesis file
  --chain-config PATH   Custom subnet-evm chain config file
  --config PATH         Config file (see config.example.json)
  --data-dir PATH       Data directory (default: ~/.avalanche-benchmark)
```

### Flood Options

```bash
benchmark flood [flags]
  --keys N    Parallel sender accounts (default: 600)
  --batch N   Transactions per batch (default: 50)
```

## Configuration

### config.example.json

```json
{
  "primaryNodeCount": 2,
  "l1ValidatorNodeCount": 2,
  "l1RpcNodeCount": 1,
  "nodeFlags": {
    "log-level": "warn"
  }
}
```

### Default Genesis (feeConfig)

| Setting | Value | Purpose |
|---------|-------|---------|
| `gasLimit` | 500M | ~23,800 transfers/block max |
| `targetBlockRate` | 1 | 1-second blocks |
| `minBaseFee` | 1 wei | Lowest possible fee |
| `targetGas` | MaxUint64 | Fees never increase |

### Default Chain Config

| Setting | Value | Purpose |
|---------|-------|---------|
| `trie-clean-cache` | 100GB | Large state cache |
| `tx-pool-global-slots` | 262K | Large pending pool |
| `skip-tx-indexing` | true | Faster execution |
| `push-gossip-frequency` | 50ms | Fast tx propagation |

## Theoretical Limits

```
Gas limit:      500M per block
Transfer cost:  21,000 gas
Block size:     1.8MB max

Max by gas:     ~23,800 TPS
Max by size:    ~16,000 TPS (actual limit)
```

## Directory Structure

```
avalanche-benchmark/
├── cmd/
│   ├── benchmark/       # CLI (start, flood, monitor, shutdown, status, logs)
│   └── bombard/         # Standalone transaction flooder
├── internal/
│   ├── config/          # Configuration loading
│   ├── network/         # Node lifecycle, L1 creation
│   ├── flood/           # Bombard process management
│   ├── bombard/         # Transaction flooding library
│   └── monitor/         # Real-time TPS metrics display
├── scripts/             # Deployment scripts for remote servers
├── staking/local/       # Pre-generated staking keys
├── config.example.json  # Example configuration
└── Makefile
```

## Environment Variables

| Variable | Description |
|----------|-------------|
| `AVALANCHEGO_PATH` | Path to avalanchego binary |
| `AVALANCHEGO_PLUGIN_DIR` | Path to plugins directory |
| `EVMBOMBARD_PATH` | Path to bombard binary |

## Remote Deployment

For deploying to remote servers:

```bash
# Build for Linux
GOOS=linux GOARCH=amd64 make build

# Copy to server
scp bin/benchmark bin/bombard ubuntu@server:~/
scp -r staking scripts ubuntu@server:~/

# On server: run setup scripts
bash ~/scripts/setup-all.sh
source ~/.benchmark-env
./benchmark start --l1-validators 5 --l1-rpcs 2
```

The `scripts/` directory contains:
- `setup-all.sh` - Master setup script
- `setup-avalanchego.sh` - Downloads AvalancheGo + subnet-evm
- `setup-env.sh` - Configures environment variables

## Make Targets

```bash
make build      # Build benchmark + bombard
make clean      # Remove build artifacts
make test       # Run tests
make package    # Create distribution tarball
```

## Troubleshooting

| Error | Solution |
|-------|----------|
| `avalanchego not found` | Set `AVALANCHEGO_PATH` or copy to `./bin/` |
| `subnet-evm plugin not found` | Set `AVALANCHEGO_PLUGIN_DIR` |
| `bombard not found` | Run `make build` |
| Ports in use | Kill processes on 9650+ |
| Low TPS | Increase `--keys`, check genesis config |
| Network already running | Run `benchmark shutdown` first |

## License

Apache 2.0
