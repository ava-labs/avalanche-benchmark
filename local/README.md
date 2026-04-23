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

## RPC URL

Once the network is up, the RPC URL is available in two places:

- **stdout** — `startnetwork` prints `RPC endpoint: <url>` right before exiting (with `--exit-on-success`) or before the metrics loop.
- **`network_data/rpcs.txt`** — a comma-separated list of every L1 node's RPC URL (validators + any dedicated RPC nodes). `bombard` reads this file to spread load across nodes.

Format: `http://<host>:<port>/ext/bc/<chainID>/rpc`.

## Notes

- `targetGas` and `baseFeeChangeDenominator` in genesis are set to uint64/int64 max to keep the base fee flat. Do not edit these with JS — `JSON.parse` loses precision and silently rounds them, which breaks fee math.
- Going below 100ms block delay is possible but depends on hardware. Ava Labs tested down to 80ms internally.
