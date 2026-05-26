# Start P-Chain Design Notes

This spec captures the current decisions for the first `benchctl` command:

```sh
./benchctl start-pchain
```

The goal is to start the local P-Chain/Primary Network processes on the benchmark host. This is the foundation for the later `create-l1` and remote L1 node startup commands.

## Step Goals

After reading this file, an agent should be able to implement the first useful runtime command without needing prior chat context:

```sh
./benchctl start-pchain
```

The command must:

- run from the runtime working directory on the **benchmark host**;
- require a local `.env` file and read `BENCHMARK_HOST_IP`;
- kill existing AvalancheGo processes with `pkill -f avalanchego || true`;
- wipe `./runtime-data/` completely;
- start exactly two local P-Chain AvalancheGo processes;
- use embedded committed test staking keys:
  - `pchain/1` for `pchain-1`
  - `pchain/2` for `pchain-2`
- run both P-Chain nodes with `--sybil-protection-enabled=false`;
- bind HTTP to `127.0.0.1`;
- set `--public-ip` from `BENCHMARK_HOST_IP`;
- use hard-coded ports:
  - `pchain-1`: HTTP `9650`, staking `9651`
  - `pchain-2`: HTTP `9652`, staking `9653`
- bootstrap `pchain-2` to `pchain-1`;
- wait for both `/ext/health` endpoints to become healthy;
- query both node IDs and verify them against embedded expected P-Chain NodeIDs;
- print concise success output with RPC URLs, expected NodeIDs, and stdout log paths.

The implementation should also set up a reusable internal node preparation/start helper because later commands will start L1 validator/RPC nodes using the same operational pattern.

Keep the orchestration app skinny. `benchctl` should orchestrate files/processes and verify the few facts required to continue. Do not add validation layers for things AvalancheGo will validate on startup. Every function needs to justify its existence.

Avoid runtime flags and overrides aggressively. If AvalancheGo has a useful default derived from `--data-dir`, use that default. Add an AvalancheGo flag only when the benchmark cannot work correctly without it.

For filesystem layout, `--data-dir` is the only path/dir flag that should normally be passed. Do not pass staking, plugin, DB, log, chain-data, or similar path flags when AvalancheGo can derive them from `--data-dir`.

Use `~/avalanche-benchmark-bombard-ws-worker-pool/local/` as the closest startup behavior reference. Keep the same operational semantics, but do not mechanically copy the old path-heavy flag list.

## Non-Goals For This Step

Do not implement these in the first step:

- `create-l1`;
- remote L1 validator startup;
- RPC node startup;
- bombard;
- failover/key-swap;
- Terraform topology/behavior changes;
- custom Primary Network genesis with sybil protection enabled;
- runtime flags/configurability beyond values that cannot work without environment input.
- extra AvalancheGo flags that only restate defaults.

## Re-Entry Notes For Future Agents

If this conversation is compacted, re-read this file before continuing. `benchctl start-pchain` and the first two wrapper scripts are already implemented. The next likely implementation is L1 creation/startup, not the full benchmark runner.

The repo already has committed benchmark test keys under `staking/`, but runtime packaging should avoid shipping those small files separately. Embed the key material into `benchctl` and write it into each node's `runtime-data/<node>/staking/` directory at startup.

The current branch may already be ahead of origin due to the key commit. Do not push unless asked.

## Terms

- **Benchmark host**: the machine where `benchctl` and bombard run. In Terraform/AWS, this is `BENCHMARK_HOST_IP` and also the first entry of `DC1_NODE_IPS`. This is a benchmark/orchestration host, not a benchmark validator role.
- **P-Chain processes**: two local AvalancheGo processes running on the benchmark host. They create the local Primary Network/P-Chain used to issue L1 creation transactions.
- **Data machines**: benchmark machines excluding the benchmark host. In the current Terraform single-DC shape, these are `DC1_NODE_IPS[1:]`.
- **L1 validator nodes**: AvalancheGo processes on data machines that run the Subnet-EVM L1 using registered L1 staking identities.
- **RPC nodes**: AvalancheGo processes on data machines that track the L1 but are not registered L1 validators.

## Decided Q&A

### Q1. Should P-Chain nodes use `--sybil-protection-enabled=false`?

Yes. Keep old `remote-failover` behavior for now.

Reason: this gets local P-Chain/L1 creation working fastest and avoids needing a custom local Primary Network genesis that admits the two P-Chain validator certs.

### Q2. Should `start-pchain` run locally on the benchmark host or SSH into `BENCHMARK_HOST_IP`?

`benchctl start-pchain` runs on the benchmark host itself.

For AWS development, the outer workflow ships the runtime package to the US West benchmark host and runs `benchctl` there. For the client, the employee's wired benchmark host runs it directly.

### Q3. Should `benchctl` build/download AvalancheGo or plugins?

No. `benchctl` is a shipped runtime binary. Building is Makefile/package responsibility.

`benchctl` must fail clearly if required runtime binaries are missing.

### Q4. Where should runtime state live?

Use a local directory:

```text
./runtime-data/
```

The operator should `cd` into the package/work directory before running `benchctl`.

### Q5. Should `start-pchain` wipe existing runtime data?

Yes. `start-pchain` always wipes the full `./runtime-data/` directory. No reset flag, no prompt.

### Q6. Should `start-pchain` kill existing AvalancheGo processes first?

Yes. Use:

```sh
pkill -f avalanchego || true
```

This is accepted even though it is broad.

### Q7. How should P-Chain HTTP and staking/gossip bind?

HTTP should be local only:

```text
--http-host=127.0.0.1
```

Staking/gossip should be reachable through the node public IP. Do not add unnecessary bind flags unless required by AvalancheGo.

### Q8. How should `benchctl` discover the benchmark host IP?

Read `.env` and use:

```text
BENCHMARK_HOST_IP
```

Manual/local mode can set `BENCHMARK_HOST_IP=127.0.0.1`.

### Q9. What env var names the benchmark host IP?

Use `BENCHMARK_HOST_IP`.

This matches current Terraform output and is clearer.

### Q10. Should P-Chain node count be configurable?

No. Hard-code exactly two P-Chain nodes because there are exactly two committed P-Chain key sets.

### Q11. Should P-Chain runtime prep copy the Subnet-EVM plugin too?

No. The local P-Chain processes do not track the L1 and do not need the Subnet-EVM plugin.

The Makefile still builds and packages the plugin because later L1 validator/RPC processes need it.

### Q12. What should P-Chain data/log layout be?

Use only `--data-dir` and let AvalancheGo place its internal DB/log/config/staking files by default:

```text
./runtime-data/pchain-1
./runtime-data/pchain-2
```

Redirect process stdout/stderr to:

```text
./runtime-data/pchain-1/stdout.log
./runtime-data/pchain-2/stdout.log
```

### Q13. Should `start-pchain` write `runtime-data/pchain.env`?

No.

Hard-code what can be hard-coded. Later commands can use fixed ports and `staking/node-ids.env` / embedded key metadata.

### Q14. Should `start-pchain` verify expected NodeIDs?

Yes. After startup, query `info.getNodeID` from each local P-Chain node and compare with the expected P-Chain NodeIDs.

Mismatch is a hard failure.

### Q15. Should `benchctl` require the repo root as cwd?

No repo-root assumption for the runtime package. The operator runs `benchctl` from the runtime working directory.

Runtime data is relative to cwd:

```text
./runtime-data/
```

### Q16. How should runtime binaries be packaged?

Make should produce runtime artifacts in the current directory and an archive containing a manually selected file list.

Runtime package should contain only a few top-level binaries plus config:

```text
benchctl
avalanchego
srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
config/genesis.json
config/chain-config.json
config/node-config.json
.env.example
scripts/00_copy-artifacts.sh
scripts/01_start-pchain.sh
scripts/02_create-l1.sh
scripts/lib.sh
```

Do not archive the whole source tree.

### Q17. Should `.env` be included in the client archive?

No. Include `.env.example`, not `.env`.

For AWS/dev, ship the real `.env` separately to the benchmark host. For the client, they fill `.env` from their pre-provisioned machine inventory.

`start-pchain` requires `.env`, even though it currently only needs `BENCHMARK_HOST_IP`, because subsequent commands will need the node inventory very soon.

### Q18. Should the artifact copy script build anything?

No. Build and artifact shipping are separate steps.

`make` is the only build step. It produces the runtime binaries and archive on a machine with Go/build tooling.

`scripts/00_copy-artifacts.sh` only checks that unpacked prebuilt runtime artifacts exist, then copies them to the benchmark host. If `BENCHMARK_HOST_IP` is `127.0.0.1`, `localhost`, or `::1`, it does nothing after checking artifacts.

`scripts/01_start-pchain.sh` starts the P-Chain on the benchmark host. If `BENCHMARK_HOST_IP` is local, it runs `./benchctl start-pchain` directly. Otherwise it SSHes to the benchmark host and runs `./benchctl start-pchain` in `/data/avalanche-benchmark`.

`scripts/lib.sh` contains the shared local-or-SSH execution helper used by runtime wrapper scripts.

The copy script must fail if artifacts are missing instead of trying to build. Client machines may not have build tools.

### Q24. What is the minimal AvalancheGo flag set for P-Chain startup?

Use the old `local/` startup semantics as the reference, but remove path flags that are redundant with `--data-dir`.

`pchain-1`:

```text
--data-dir=./runtime-data/pchain-1
--config-file=./runtime-data/pchain-1/node-config.json
--network-id=local
--sybil-protection-enabled=false
--http-host=127.0.0.1
--http-port=9650
--staking-port=9651
--public-ip=<BENCHMARK_HOST_IP>
--bootstrap-ips=
--bootstrap-ids=
```

`pchain-2`:

```text
--data-dir=./runtime-data/pchain-2
--config-file=./runtime-data/pchain-2/node-config.json
--network-id=local
--sybil-protection-enabled=false
--http-host=127.0.0.1
--http-port=9652
--staking-port=9653
--public-ip=<BENCHMARK_HOST_IP>
--bootstrap-ips=<BENCHMARK_HOST_IP>:9651
--bootstrap-ids=<PCHAIN_1_NODE_ID>
```

Do not pass:

```text
--db-dir
--log-dir
--chain-data-dir
--plugin-dir
--staking-tls-cert-file
--staking-tls-key-file
--staking-signer-key-file
```

### Q25. What readiness checks should `start-pchain` perform?

Keep checks skinny:

- wait for `/ext/health`;
- call `info.getNodeID`;
- compare node IDs with the expected embedded P-Chain NodeIDs.

Do not add P-Chain-specific API probes such as `platform.getHeight` in this step.

## Current Runtime Contract

Required files in the runtime package / artifact copy workflow:

```text
./benchctl
./avalanchego
./srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
./config/genesis.json
./config/chain-config.json
./config/node-config.json
./.env
```

`benchctl start-pchain` itself currently requires only:

```text
./benchctl
./avalanchego
./config/node-config.json
./.env
```

The Subnet-EVM plugin, genesis, and chain config are packaged now because later L1 commands need them.

Staking keys are committed in source and should be embedded into `benchctl` to avoid shipping many small key files.

`benchctl start-pchain` should write embedded P-Chain staking keys into:

```text
./runtime-data/pchain-1/staking/
./runtime-data/pchain-2/staking/
```

Write the embedded keys using AvalancheGo's default staking filenames:

```text
staker.crt
staker.key
signer.key
```

Do not pass staking path flags:

```text
--staking-tls-cert-file
--staking-tls-key-file
--staking-signer-key-file
```

For `start-pchain`, copy only `config/node-config.json` into each P-Chain runtime directory and pass that runtime-local file to AvalancheGo with `--config-file`.

Do not copy `config/genesis.json` during `start-pchain`; it is used later by L1 creation.

Do not copy `config/chain-config.json` during `start-pchain`; chain config is only useful after the L1 chain ID exists and belongs in the chain-specific config path.

AvalancheGo source check for later L1 node startup:

- `config/flags.go` defines the default plugin dir as `<data-dir>/plugins`.
- `config/config.go:getPluginDir` creates the default plugin dir when `--plugin-dir` is not explicitly set.
- `vms/registry/vm_getter.go` reads files directly from the plugin directory, skips subdirectories, and treats each filename as an alias or VM ID.

Therefore later L1 node startup should make the Subnet-EVM plugin available at:

```text
<l1-node-data-dir>/plugins/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
```

and should not pass `--plugin-dir`.

`start-pchain` should not copy or require the plugin.

## Hard-Coded P-Chain Ports

```text
pchain-1 HTTP:    9650
pchain-1 staking: 9651
pchain-2 HTTP:    9652
pchain-2 staking: 9653
```

`pchain-2` bootstraps to `pchain-1` using:

```text
BENCHMARK_HOST_IP:9651
PCHAIN_1_NODE_ID
```

For later L1 node startup, bootstrap to both local P-Chain nodes using hard-coded P-Chain IDs and `.env` benchmark host IP:

```text
--bootstrap-ips=<BENCHMARK_HOST_IP>:9651,<BENCHMARK_HOST_IP>:9653
--bootstrap-ids=<PCHAIN_1_NODE_ID>,<PCHAIN_2_NODE_ID>
```

## Script Workflow

From a source checkout with build tools:

```sh
make
# ensure .env exists and is filled
./scripts/00_copy-artifacts.sh
./scripts/01_start-pchain.sh
```

From an unpacked runtime package:

```sh
# create .env from .env.example first
./scripts/00_copy-artifacts.sh
./scripts/01_start-pchain.sh
```

`00_copy-artifacts.sh` is copy-only. It never runs `make`.

`01_start-pchain.sh` exits when `benchctl start-pchain` exits. A successful exit means both P-Chain processes are healthy and have the expected NodeIDs.
