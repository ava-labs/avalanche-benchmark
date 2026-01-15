# Avalanche Benchmark CLI

Benchmark Avalanche L1 (subnet-evm) throughput. Zero build dependencies on target.

## Deploy to Server

**Requirements:** 4 binaries, no build tools needed.

### 1. Build (on dev machine)

```bash
git clone https://github.com/ava-labs/avalanche-benchmark.git
cd avalanche-benchmark
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o benchmark-linux ./cmd/benchmark
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bombard-linux ./cmd/bombard
```

### 2. Copy to server

```bash
scp benchmark-linux bombard-linux ubuntu@server:~/
scp -r staking scripts ubuntu@server:~/
```

### 3. Setup

```bash
ssh ubuntu@server
mv benchmark-linux benchmark && mv bombard-linux bombard && chmod +x benchmark bombard
bash ~/scripts/setup-all.sh
source ~/.benchmark-env
```

### 4. Run

```bash
./benchmark start --l1-validators 5 --l1-rpcs 2
./benchmark flood
./benchmark monitor
./benchmark shutdown
```

## Offline Deployment

For servers without internet, pre-download avalanchego and subnet-evm:

```bash
# On dev machine
wget https://github.com/ava-labs/avalanchego/releases/download/v1.14.1/avalanchego-linux-amd64-v1.14.1.tar.gz
wget https://github.com/ava-labs/subnet-evm/releases/download/v0.8.0/subnet-evm_0.8.0_linux_amd64.tar.gz
tar -xzf avalanchego-linux-amd64-v1.14.1.tar.gz
tar -xzf subnet-evm_0.8.0_linux_amd64.tar.gz

# Copy all to server
scp benchmark-linux bombard-linux ubuntu@server:~/
scp avalanchego-v1.14.1/avalanchego ubuntu@server:~/
scp subnet-evm ubuntu@server:~/
scp -r staking ubuntu@server:~/

# On server
mkdir -p ~/.avalanchego/plugins
mv ~/avalanchego ~/.avalanchego/
mv ~/subnet-evm ~/.avalanchego/plugins/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
echo 'export AVALANCHEGO_PATH=~/.avalanchego/avalanchego' >> ~/.bashrc
echo 'export AVALANCHEGO_PLUGIN_DIR=~/.avalanchego/plugins' >> ~/.bashrc
echo 'export EVMBOMBARD_PATH=~/bombard' >> ~/.bashrc
source ~/.bashrc
```

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

### Options

```bash
benchmark start --l1-validators 5 --l1-rpcs 2    # Scale validators/RPC nodes
benchmark flood --keys 600 --batch 50            # Tune flooding
```

## Config File

```json
{
  "primaryNodeCount": 2,
  "l1ValidatorNodeCount": 5,
  "l1RpcNodeCount": 2,
  "nodeFlags": { "log-level": "warn" }
}
```

```bash
benchmark start --config config.json
```

## Performance

```
Max TPS: ~16,000 (limited by 1.8MB block size)
Gas limit: 500M per block
Block time: 1 second
```

## Troubleshooting

| Error | Fix |
|-------|-----|
| `avalanchego not found` | Run `scripts/setup-all.sh` |
| `bombard not found` | Set `EVMBOMBARD_PATH=~/bombard` |
| Ports in use | `benchmark shutdown` or reboot |

## License

Apache 2.0
