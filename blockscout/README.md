## Blockscout

Optional explorer for the benchmark chain. Runs as a 4-container compose stack
(Blockscout backend + frontend, Postgres, Redis) using **podman** by default
and **docker** as a fallback.

The stack can target either:

- a `local` benchmark chain started from this repo
- a `remote` benchmark chain reached over the same RPC URL you use for benchmarking

## What this builds

`scripts/blockscout-pack-images.sh` pulls the four upstream images on a build
machine with internet access and saves them into a single OCI archive at
`blockscout/images.tar.gz`. `make pack-blockscout` (in `local/` or `remote/`)
bundles that archive together with the compose file, helper scripts, and the
existing benchmark payload into one `*-with-blockscout.tar.gz`. On the target
machine, `./blockscout.sh up` runs `podman load -i blockscout/images.tar.gz`
on first call and then starts the compose stack — no registry traffic required.

The local and remote networks expose a dedicated **Archive-RPC** avalanchego
node bound to `0.0.0.0` with `--http-allowed-hosts=*` and `pruning-enabled:
false`. Blockscout queries this node so its container Host headers are
accepted and historical state lookups succeed. The bombard RPC nodes stay on
loopback with the default chain config.

## Online (dev box) flow

```bash
cd local
./bin/startnetwork --exit-on-success
./blockscout.sh up
./blockscout.sh smoke
./blockscout.sh down
```

If you want a one-step convenience flow:

```bash
cd local
./blockscout.sh up --start-local
```

For remote:

```bash
cd remote
./blockscout.sh up
./blockscout.sh smoke
./blockscout.sh down
```

The `remote` wrapper reads `network.env`, picks up the `ARCHIVE_RPC_URL` set
by `03_deploy_l1_config.sh`, and runs the same compose stack against that
endpoint.

## Air-gapped RHEL flow

The deliverable tarball ships the OCI archive inside, so the target machine
needs no registry access.

### 1. On a build machine (with internet)

```bash
# Build the image bundle once. Skip if blockscout/images.tar.gz already exists.
./scripts/blockscout-pack-images.sh

# Then bundle it with the rest of the deliverable. Pick one:
cd local  && make pack-blockscout      # local-benchmark-with-blockscout.tar.gz
cd remote && make pack-blockscout      # remote-benchmark-with-blockscout.tar.gz
```

The image bundle defaults to `linux/amd64`. Override with
`BLOCKSCOUT_PLATFORM=linux/arm64` for ARM RHEL hosts.

### 2. On the air-gapped RHEL target

Prereqs (install from your offline RPM mirror):

```bash
sudo dnf install podman podman-compose
```

Podman 4.7+ is required (RHEL 9.3+ ships 4.7+; RHEL 9.4+ ships 5.x).

Then:

```bash
tar -xzf local-benchmark-with-blockscout.tar.gz
./bin/startnetwork --exit-on-success
./blockscout.sh up           # auto-loads images from blockscout/images.tar.gz
./blockscout.sh smoke
./blockscout.sh down
```

`blockscout.sh up` is idempotent: it inspects the local image cache first and
only runs `podman load` when an image is missing.

### Default URLs

- Frontend: `http://127.0.0.1:4001`
- Backend API: `http://127.0.0.1:4000/api/v2/stats`

### Image overrides

Image pins live in [`images.conf`](./images.conf). Override via env vars at
pack time or run time:

```bash
BLOCKSCOUT_BACKEND_IMAGE=ghcr.io/your-org/blockscout-backend:<tag> \
BLOCKSCOUT_FRONTEND_IMAGE=ghcr.io/your-org/blockscout-frontend:<tag> \
./scripts/blockscout-pack-images.sh
```

### Runtime override

Force a specific runtime if both are installed:

```bash
BLOCKSCOUT_RUNTIME=docker ./blockscout.sh up
BLOCKSCOUT_RUNTIME=podman ./blockscout.sh up
```

### Shared scripts

- `scripts/blockscout-pack-images.sh` — pull + save images for offline use
- `scripts/blockscout-up.sh` — bring the stack up (also accepts `--rpc URL`)
- `scripts/blockscout-down.sh` — tear down + cleanup
- `scripts/blockscout-smoke.sh` — basic API/frontend reachability check
- `scripts/_blockscout_runtime.sh` — sourced helper, picks the runtime + compose CLI
