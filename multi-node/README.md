# Multi-Node Benchmark

Benchmark tool for 3-node Avalanche L1 network.

## Ports

Open the following ports on your nodes:

| Port | Service | Required | Notes |
|------|---------|----------|-------|
| 22 | SSH | Yes | Remote access |
| 9650-9655 | AvalancheGo | Yes | HTTP API + Staking ports for primary/validator/RPC nodes |
| 3000 | Grafana | Optional | Monitoring dashboard (node1 only) |
| 9090 | Prometheus | No | Grafana queries locally; only needed for external access |

## Setup

```bash
# Configure SSH user and node IPs
cp .env.example .env
# Edit .env:
#   SSH_USER=ubuntu        # SSH username for all nodes
#   NODE1_IP=1.2.3.1
#   NODE2_IP=1.2.3.2
#   NODE3_IP=1.2.3.3
```

## Usage

```bash
# 1. Start 3-node primary network
./01_bootstrap_primary_network.sh

# 2. Create L1 (subnet + chain + validators)
./02_create_l1.sh

# 3. Deploy monitoring (optional)
./03_monitoring.sh

# 4. Run benchmark
./04_benchmark.sh

# Cleanup
./05_cleanup.sh
```

## Benchmark Options

```bash
./04_benchmark.sh                         # default (500 keys, 500 batch)
./04_benchmark.sh -batch 1000             # larger batches
./04_benchmark.sh -keys 1000              # more parallel senders
./04_benchmark.sh -erc20                  # ERC20 transfers
./04_benchmark.sh -both                   # alternate native/ERC20
./04_benchmark.sh -keys 1000 -batch 1000  # high throughput
```
