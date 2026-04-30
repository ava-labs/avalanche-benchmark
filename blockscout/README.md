## Blockscout

This repo includes a simple Blockscout sidecar stack for the benchmark chain.

The stack always runs on the operator machine via Docker. It can point at:

- a `local` benchmark chain started from this repo
- a `remote` benchmark chain reached over the same RPC URL you already use for benchmarking

### Local

```bash
cd local
./bin/startnetwork --exit-on-success
./blockscout.sh up
./blockscout.sh smoke
./blockscout.sh down
```

If you want a one-step convenience flow instead, you can opt in explicitly:

```bash
cd local
./blockscout.sh up --start-local
```

### Remote

```bash
cd remote
./blockscout.sh up
./blockscout.sh smoke
./blockscout.sh down
```

The remote wrapper reads `network.env`, builds the remote L1 RPC URL, and then launches the same local Docker stack against that RPC endpoint.

### Shared Scripts

The wrappers call the shared root scripts:

- `scripts/blockscout-up.sh`
- `scripts/blockscout-smoke.sh`
- `scripts/blockscout-down.sh`

You can also run the shared launcher directly:

```bash
./scripts/blockscout-up.sh --rpc http://HOST:9654/ext/bc/CHAIN_ID/rpc
```

### Default URLs

- Frontend: `http://127.0.0.1:4001`
- Backend API: `http://127.0.0.1:4000/api/v2/stats`

### Image Overrides

The compose stack defaults to upstream Blockscout images because the Avalanche-specific image repos referenced in the `avalanche-deploy` Blockscout issue are not publicly pullable yet.

You can override them at runtime:

```bash
BLOCKSCOUT_BACKEND_IMAGE=avaplatform/blockscout-backend:<tag> \
BLOCKSCOUT_FRONTEND_IMAGE=avaplatform/blockscout-frontend:<tag> \
./scripts/blockscout-up.sh
```
