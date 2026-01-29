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

# 2. Create L1 (subnet + chain) - saves SUBNET_ID/CHAIN_ID to network.env
./02_create_l1.sh

# 3. Deploy chain config and start validator/RPC nodes
./03_deploy_l1_config.sh

# 4. Deploy monitoring (optional)
./04_monitoring.sh

# 5. Run benchmark
./05_benchmark.sh

# Cleanup
./06_cleanup.sh
```

To apply a new chain config without recreating the L1:
```bash
# Edit chain-config.json, then:
./03_deploy_l1_config.sh
```

## Benchmark Options

```bash
./05_benchmark.sh              # default 4000 TPS target
./05_benchmark.sh -tps 6000    # higher TPS target
./05_benchmark.sh -tps 2000    # lower TPS target
```
