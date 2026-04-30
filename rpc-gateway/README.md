# RPC Gateway PoC

This directory contains a proof-of-concept Web2 API layer in front of an Avalanche EVM RPC.

The gateway keeps the client-facing interface as normal JSON-RPC while enforcing policy before forwarding requests upstream. It is intended to run alongside the benchmark network in this repo and auto-detect the upstream L1 RPC from `network_data/rpcs.txt`.

## What It Enforces

- API key authentication via `X-API-Key` or `Authorization: Bearer ...`
- Source IP allowlists
- RPC method allowlists
- Per-key fixed-window request limits
- `eth_call` and `eth_estimateGas` inspection
- `eth_sendRawTransaction` inspection:
  - recovered `from`
  - `to`
  - function selector
  - gas limit
  - value
  - chain ID
  - contract creation toggle

## Layout

- `cmd/rpc-gateway`: binary entrypoint
- `migrations/001_init.sql`: Postgres schema
- `examples/demo-seed.sql`: sample tenant and key record
- `examples/bombard-seed.sql`: broader local benchmark policy that works with `bombard`
- `docker-compose.yml`: local Postgres for the PoC

## Run

For the simplest local demo flow from the repo root:

```bash
./scripts/rpc-gateway-demo-up.sh
source ./tmp/rpc-gateway-demo/demo.env
./scripts/rpc-gateway-demo-smoke.sh
./scripts/rpc-gateway-demo-bombard.sh -erc20 -tps 250
./scripts/rpc-gateway-demo-down.sh
```

Manual setup is still available below if you want to inspect each step.

1. Start the benchmark network from `single-node` so `single-node/network_data/rpcs.txt` exists.

2. Start Postgres:

```bash
cd rpc-gateway
make db-up
```

3. Apply the schema:

```bash
make migrate
```

4. Generate an API key and keep the printed `raw_key` value:

```bash
make keygen
```

5. Seed either the narrow demo policy or the broader benchmark policy:

```bash
RAW_KEY=REPLACE_WITH_RAW_KEY make seed-demo
# or:
RAW_KEY=REPLACE_WITH_RAW_KEY make seed-bombard
```

6. Start the gateway:

```bash
make run
```

If `UPSTREAM_RPC_URL` is unset, the gateway will use the first URL it finds in `../single-node/network_data/rpcs.txt`, `../network_data/rpcs.txt`, or `./network_data/rpcs.txt`.

## Example Calls

Read call:

```bash
curl -s http://127.0.0.1:8080/rpc \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: REPLACE_WITH_RAW_KEY' \
  --data '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}'
```

Denied write example:

- The sample policy only allows ERC-20 `transfer(address,uint256)` calls to `0xB0B5...`.
- A native transfer or a write to a different contract will be rejected before it reaches the node.

`bombard` through the gateway:

```bash
cd ../single-node
go run ./cmd/bombard \
  -rpc http://127.0.0.1:8080/rpc \
  -api-key REPLACE_WITH_RAW_KEY \
  -erc20 \
  -tps 250
```

The `demo` seed is intentionally narrow. The `bombard` seed is broader on writes so the benchmark client can fund worker accounts and then send load through the gateway.

## Policy Shape

Policies are stored as JSON in `tenants.policy`.

```json
{
  "allowedCidrs": ["127.0.0.1/32"],
  "allowedMethods": [
    "eth_chainId",
    "eth_blockNumber",
    "eth_getBalance",
    "eth_call",
    "eth_estimateGas",
    "eth_sendRawTransaction"
  ],
  "allowedFromAddresses": ["0x8db97C7ceCe249c2b98bdC0226Cc4C2A57BF52FC"],
  "allowedToAddresses": ["0xB0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5"],
  "allowedFunctionSelectors": ["0xa9059cbb"],
  "allowContractCreation": false,
  "maxGasLimit": 120000,
  "maxValueWei": "0",
  "requestsPerMinute": 600
}
```

## Notes

- Batch JSON-RPC requests are intentionally rejected in this first pass.
- WebSocket proxying is not implemented in this PoC.
- The gateway logs allow/deny decisions as structured JSON to stdout.
