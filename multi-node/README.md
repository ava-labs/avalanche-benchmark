# Multi-Node Benchmark

Benchmark tool for 3-node Avalanche L1 network.

## Ports

Open the following ports on your nodes:

| Port | Service | Required | Notes |
|------|---------|----------|-------|
| 22 | SSH | Yes | Remote access |
| 9650 | AvalancheGo HTTP API | Yes | RPC endpoint |
| 9651 | AvalancheGo Staking | Yes | P2P communication between nodes |
| 3000 | Grafana | Optional | Monitoring dashboard (node1 only) |
| 9090 | Prometheus | No | Grafana queries locally; only needed for external access |

## Setup

```bash
# Download dependencies and build tools
make

# Configure node IPs
cp .env.example .env
# Edit .env with your 3 node IPs
```

## Usage

```bash
# 1. Start 3-node primary network
./01_bootstrap_primary_network.sh

# 2. Create L1 (subnet + chain + validators)
./02_create_l1.sh

# 3. Run benchmark
./03_benchmark.sh

# Cleanup
./05_cleanup.sh
```

## Benchmark Options

```bash
./03_benchmark.sh                         # default (500 keys, 500 batch)
./03_benchmark.sh -batch 1000             # larger batches
./03_benchmark.sh -keys 1000              # more parallel senders
./03_benchmark.sh -erc20                  # ERC20 transfers
./03_benchmark.sh -both                   # alternate native/ERC20
./03_benchmark.sh -keys 1000 -batch 1000  # high throughput
```
