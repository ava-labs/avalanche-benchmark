Uses a custom avalanchego build from the `configure-genesis-acp226-excess` branch that adds `initialMinDelayMS` to the genesis config. This lets new chains start at the target block delay immediately, skipping the ~3-hour ACP-226 ramp-down.

## Config

Three things must be set for fast blocks:

**genesis.json:**
- `graniteTimestamp: 0` — enables Granite (ACP-226) at genesis
- `initialMinDelayMS: 100` — target block delay in ms
- `timestamp` — must be a recent Unix epoch (hex), not `0x0`. `0x0` = year 1970, before Granite activation, so the config gets silently ignored

**chain-config.json:**
- `min-delay-target: 100` — must match `initialMinDelayMS`

## Build & Run

`make pack` to build everything and pack into `local-benchmark.tar.gz`.

On the target machine:
```bash
tar -xzf local-benchmark.tar.gz
./bin/startnetwork --exit-on-success
./bin/bombard --tps 300
```

### Optional Explorer

To run Blockscout against the benchmark chain on the same host:

```bash
./bin/startnetwork --exit-on-success
./blockscout.sh up
./blockscout.sh smoke
./blockscout.sh down
```

This launches the Blockscout compose stack and points it at the local
benchmark RPC. It uses **podman** if installed (RHEL default), otherwise
**docker**.

If you want the explorer script to start the local network for you, use:

```bash
./blockscout.sh up --start-local
```

### Air-gapped (RHEL) deployment

For a target with no internet, build a self-contained tarball that includes
saved OCI images for the 4 Blockscout containers:

```bash
make pack-blockscout    # produces local-benchmark-with-blockscout.tar.gz
```

The tarball extracts flat (same layout as `local-benchmark.tar.gz`). On the
target, install `podman` and `podman-compose` from your offline RPM mirror,
then:

```bash
tar -xzf local-benchmark-with-blockscout.tar.gz
./bin/startnetwork --exit-on-success
./blockscout.sh up           # auto-loads images from blockscout/images.tar.gz
```

See [`../blockscout/README.md`](../blockscout/README.md) for full details on
the offline flow, image overrides, and runtime selection.

## RPC URLs

Once the network is up, RPC URLs are available in two files plus stdout:

- **stdout** — `startnetwork` prints `RPC endpoint: <url>` right before exiting (with `--exit-on-success`) or before the metrics loop.
- **`network_data/rpcs.txt`** — a comma-separated list of every loopback-bound L1 node's RPC URL (validators + dedicated RPC nodes). `bombard` reads this file to spread load across nodes.
- **`network_data/archive-rpcs.txt`** — only present when `l1ArchiveRpcs >= 1` in `benchmark-config.json`. Contains the URL of the dedicated Archive-RPC node bound to `0.0.0.0` with `pruning-enabled: false`. `blockscout-up.sh` prefers this over `rpcs.txt`.

Format: `http://<host>:<port>/ext/bc/<chainID>/rpc`.

## Notes

- `targetGas` and `baseFeeChangeDenominator` in genesis are set to uint64/int64 max to keep the base fee flat. Do not edit these with JS — `JSON.parse` loses precision and silently rounds them, which breaks fee math.
- Going below 100ms block delay is possible but depends on hardware. Ava Labs tested down to 80ms internally.
