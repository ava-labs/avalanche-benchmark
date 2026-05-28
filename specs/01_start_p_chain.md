# Start P-Chain Design Notes

This spec captures the current target design for:

```sh
./scripts/01_start-pchain.sh
```

The goal is to start two P-Chain/Primary Network AvalancheGo processes on the benchmark host. The operator machine runs the script and performs remote health checks over HTTP.

When `.env` sets `SYBIL_ENABLED_LOCAL=1`, this script uses the sybil-enabled local-genesis path instead of the older benchmark-host two-process path:

- start the first five `DC1_NODE_IPS` as the built-in local genesis Primary Network validators;
- copy `staking/l1/1..5` into those five primary data dirs;
- use ports `9650/9651` on each of those five node hosts;
- do not pass `--sybil-protection-enabled=false`;
- do not start the benchmark-host `staking/pchain/1..2` processes.

## Re-Entry Notes For Future Agents

This spec supersedes the older `benchctl start-pchain` design. If the code still starts P-Chain through `benchctl`, refactor it to this script-driven model before building later steps.

Machine roles and inventory live in `specs/00_topology_and_inventory.md`.

## Step Goals

`scripts/01_start-pchain.sh` must:

- require `.env`;
- use `BENCHMARK_HOST_IP` as the benchmark host SSH target unless it is `127.0.0.1`, `localhost`, or `::1`;
- start exactly two AvalancheGo processes on the benchmark host;
- kill existing benchmark-host AvalancheGo processes first with `pkill -f '[a]valanchego' || true`;
- wipe benchmark-host `/data/avalanche-benchmark/runtime-data`;
- copy committed P-Chain staking files into the two runtime data dirs:
  - `staking/pchain/1` for `pchain-1`
  - `staking/pchain/2` for `pchain-2`
- copy `config/node-config.json` into each P-Chain runtime data dir;
- copy the Subnet-EVM plugin into each P-Chain runtime data dir default plugin path;
- run both P-Chain nodes with `--sybil-protection-enabled=false`;
- bind P-Chain HTTP to `0.0.0.0`;
- use hard-coded ports:
  - `pchain-1`: HTTP `9650`, staking `9651`
  - `pchain-2`: HTTP `9652`, staking `9653`
- set `--public-ip` from `BENCHMARK_HOST_IP`;
- bootstrap `pchain-2` to `pchain-1`;
- wait for both remote `/ext/health` endpoints to become healthy;
- query `info.getNodeID` for both nodes over remote HTTP;
- verify expected P-Chain NodeIDs from `staking/node-ids.env`;
- print concise success output with RPC URLs and log paths.

No `benchctl` is needed on the benchmark host.

## Non-Goals For This Step

Do not implement:

- L1 creation;
- L1 validator/RPC node startup;
- `bombard`;
- failover/key-swap;
- runtime flags or CLI overrides;
- custom Primary Network genesis with sybil protection enabled.

## Decided Q&A

### Q1. Should P-Chain nodes use `--sybil-protection-enabled=false`?

Yes. Keep old `remote-failover` behavior for now.

### Q2. Who starts the P-Chain nodes?

The operator script starts them over SSH.

If `BENCHMARK_HOST_IP` is local, the same script runs the commands locally.

### Q3. Should `benchctl` run on the benchmark host?

No.

P-Chain startup is simple process/file orchestration. Shell over SSH is enough.

### Q4. Where should benchmark-host runtime state live?

Use:

```text
/data/avalanche-benchmark/runtime-data
```

P-Chain data dirs:

```text
/data/avalanche-benchmark/runtime-data/pchain-1
/data/avalanche-benchmark/runtime-data/pchain-2
```

### Q5. Should startup wipe existing runtime data?

Yes. `01_start-pchain.sh` wipes the full benchmark-host runtime data directory. No reset flag, no prompt.

### Q6. Should startup kill existing AvalancheGo processes first?

Yes:

```sh
pkill -f '[a]valanchego' || true
```

### Q7. How should P-Chain HTTP bind?

Use:

```text
--http-host=0.0.0.0
```

Reason: `create-l1` runs on the operator machine and talks to `http://<BENCHMARK_HOST_IP>:9650`.

### Q8. Should P-Chain node count be configurable?

No. Hard-code exactly two P-Chain nodes because there are exactly two committed P-Chain key sets.

### Q9. Should P-Chain runtime prep copy the Subnet-EVM plugin?

Yes. AvalancheGo source shows that when `--sybil-protection-enabled=false`, PlatformVM creates all subnet chains it sees, not only explicitly tracked subnets. After `create-l1`, the two P-Chain processes will try to instantiate the L1 VM. If the Subnet-EVM plugin is absent, `/ext/health` reports `vmFactory ... was not found`.

Use the default plugin path under each P-Chain data dir. Do not pass `--plugin-dir`.

### Q10. Should startup verify expected NodeIDs?

Yes. Query `info.getNodeID` from both P-Chain nodes and compare with expected values from `staking/node-ids.env`.

Mismatch is a hard failure.

### Q11. What is the minimal AvalancheGo flag set?

`pchain-1`:

```text
--data-dir=/data/avalanche-benchmark/runtime-data/pchain-1
--config-file=/data/avalanche-benchmark/runtime-data/pchain-1/node-config.json
--network-id=local
--sybil-protection-enabled=false
--http-host=0.0.0.0
--http-port=9650
--staking-port=9651
--public-ip=<BENCHMARK_HOST_IP>
--bootstrap-ips=
--bootstrap-ids=
```

`pchain-2`:

```text
--data-dir=/data/avalanche-benchmark/runtime-data/pchain-2
--config-file=/data/avalanche-benchmark/runtime-data/pchain-2/node-config.json
--network-id=local
--sybil-protection-enabled=false
--http-host=0.0.0.0
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

Put staking files at the default path under each data dir:

```text
<data-dir>/staking/staker.crt
<data-dir>/staking/staker.key
<data-dir>/staking/signer.key
```

### Q12. What readiness checks should the script perform?

Keep checks skinny:

- wait for `/ext/health`;
- call `info.getNodeID`;
- compare node IDs with expected P-Chain NodeIDs.

Do not add P-Chain-specific API probes such as `platform.getHeight` in this step.

## Script Workflow

From a source checkout or unpacked runtime package:

```sh
# ensure .env exists and is filled
./scripts/00_copy-artifacts.sh
./scripts/01_start-pchain.sh
```

`01_start-pchain.sh` exits only after both P-Chain processes are healthy and have expected NodeIDs.
