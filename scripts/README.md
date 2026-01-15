# Avalanche Benchmark Setup Scripts

Scripts for deploying the benchmark in an "airtight" environment with only the required binaries.

## Files

- **setup-all.sh** - Master setup script that runs everything
- **setup-avalanchego.sh** - Downloads and installs AvalancheGo + subnet-evm plugin
- **setup-env.sh** - Configures environment variables

## Usage

### Fresh Machine Setup

```bash
# 1. Copy files to server
scp -i key.pem benchmark-linux ubuntu@server:~/
scp -i key.pem bombard-linux ubuntu@server:~/
scp -i key.pem -r staking ubuntu@server:~/
scp -i key.pem -r scripts ubuntu@server:~/

# 2. SSH into server
ssh -i key.pem ubuntu@server

# 3. Run setup
bash ~/scripts/setup-all.sh

# 4. Load environment
source ~/.benchmark-env

# 5. Start benchmark
./benchmark start --l1-validators 5 --l1-rpcs 2
```

### What Gets Installed

- **AvalancheGo v1.14.1** → `~/.avalanchego/avalanchego`
- **subnet-evm v0.8.0** → `~/.avalanchego/plugins/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy`
- **Environment file** → `~/.benchmark-env`

### Environment Variables

```bash
AVALANCHEGO_PATH=~/.avalanchego/avalanchego
AVALANCHEGO_PLUGIN_DIR=~/.avalanchego/plugins
EVMBOMBARD_PATH=~/bombard
PATH=~:$PATH
```

## Dependencies

The scripts require:
- `wget` (for downloading binaries)
- `tar` (for extracting archives)
- `bash` (for running scripts)

These are pre-installed on standard Ubuntu AMIs.

## Airtight Deployment

No external dependencies required besides:
1. The **benchmark** binary (with embedded staking keys at `./staking/local/`)
2. The **bombard** binary (for transaction flooding)
3. AvalancheGo binary
4. subnet-evm plugin

No need for:
- ❌ avalanche-cli
- ❌ Go toolchain
- ❌ Build tools
- ❌ Git repositories

All binaries can be built once on a development machine and deployed to production/benchmark environments.
