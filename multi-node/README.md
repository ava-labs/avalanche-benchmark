# Multi-Node Benchmark

Benchmark tool for N-node Avalanche L1 networks. Supports 1 to N validator nodes.

## Ports

Open the following ports on your nodes:

| Port | Service | Required | Notes |
|------|---------|----------|-------|
| 22 | SSH | Yes | Remote access |
| 9650-9655 | AvalancheGo | Yes | HTTP API + Staking ports for primary/validator/RPC nodes |
| 3000 | Grafana | Optional | Monitoring dashboard (first node only) |
| 9090 | Prometheus | No | Grafana queries locally; only needed for external access |

## Setup

```bash
# Configure SSH user and node IPs
cp .env.example .env
# Edit .env:
#   SSH_USER=ubuntu
#   NODE_IPS=1.2.3.1,1.2.3.2,1.2.3.3    # comma-separated, first = bootstrap
```

Single-node example:
```bash
NODE_IPS=1.2.3.1
```

## Build

Binaries are built from source on a Linux machine (requires Go and git):

```bash
make          # builds avalanchego from configure-genesis-acp226-excess branch + tools
```

## Usage

```bash
# 1. Start N-node primary network
./01_bootstrap_primary_network.sh

# 2. Create L1 (subnet + chain) with all nodes as validators
./02_create_l1.sh

# 3. Deploy chain config and start validator/RPC nodes
./03_deploy_l1_config.sh

# 4. Deploy monitoring (optional, runs on first node)
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
./05_benchmark.sh -erc20       # ERC20 transfers instead of native
./05_benchmark.sh -tps 4000 -erc20  # combine options
```

### ERC20 Mode

Use `-erc20` to benchmark ERC20 token transfers instead of native transfers. ERC20 transfers use more gas (~65k vs 21k for native) but 4000 TPS should still be achievable on modern hardware.

### TPS Tuning

4000 TPS is a safe starting point for modern hardware. If you want to push higher:

1. Increase by ~1000 TPS increments
2. Let each test run for at least 5 minutes to make sure the load is sustainable
3. Monitor for errors or degraded performance

If you pushed too hard and need to restart, wait 60 seconds for the mempool to clear before starting a new benchmark (mempool expiration is set to 1 minute).

### Block Time

Genesis is configured with ACP-226 excess gas parameters (`graniteTimestamp: 0`, `initialMinDelayMS: 100`) for fast block production from the start. To tune further, edit `min-delay-target` in `chain-config.json` and run `./03_deploy_l1_config.sh` again.

### Reference Benchmark

On a 3-node AWS cluster using `m6a.4xlarge` instances (16 vCPU, 64GB RAM, AMD EPYC 7R13 from 2021, 3000 IOPS gp3 disk), `-tps 7000` is a good target, achieving ~6900 actual TPS sustained. In case of ERC20 txs, 5000 would be a good target.
