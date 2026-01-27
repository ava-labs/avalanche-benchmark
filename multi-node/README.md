# Multi-Node Benchmark

Benchmark tool for 3-node Avalanche L1 network.

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
./09_cleanup.sh
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
