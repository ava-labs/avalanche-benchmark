# App: settlement-feed

A Chainlink-compatible price feed with an example settlement consumer, on top
of the base layer's L1. This is the first app in the base-plus-apps layout:
self-contained, no dependency on any other app, and everything specific to it
lives in this directory.

## What it is

- `contracts/`: `PriceAggregator` (per-pair writer, one authorized publisher)
  behind `PriceFeedProxy` (stable consumer address, phase-packed round ids,
  two-step aggregator swaps). Reads are `IPriceFeed`, signature-identical to
  Chainlink's `AggregatorV3Interface`, so consumers written against Chainlink
  feeds interoperate unchanged. `Settlement.sol` is the example consumer: a
  peg guard gating settlement on price band plus freshness.
- `cmd/oracle`: the off-chain services. `oracle feed <rpc-url>` publishes
  mock USDC-USD rounds as type-2 (priority fee) transactions and reads them
  back through the proxy; `oracle relay` is the Warp path for the optional
  oracle-L1 shape.
- `oraclecontracts/`: deployed bytecode embedded for genesis baking (see the
  package comment; this is the app's one edge into the base layer).
- `dashboards/`: the Direct Price Feed Grafana dashboard. Copy next to the
  base dashboards when provisioning Grafana.
- `docs/oracle-consumer.md`: consumer reference for teams writing against
  the feed.

## How it deploys

Two supported paths, both proven:

1. **Genesis-baked (default)**: every main-chain genesis carries the proxy at
   `0x...FeedF00D` and the aggregator at `0x...FEeDfAce`; `l1 create` bakes
   them, and `network.env` records the addresses. Nothing to deploy at
   runtime.
2. **After the fact**: deploy `Settlement` (or your own consumer) with forge
   against the running chain, as any consumer team would.

The oracle-L1 shape's configuration files (`oracle-genesis-template.json`,
`subnet-config-oracle.json`) currently live at the repository root because
the kit's runtime contract reads them from the deployment root; they belong
to this app and move here once the base layer grows a genesis/config drop-in
hook.

## Run it

```bash
./bin/oracle feed http://<rpc-host>:9650                      # publish rounds
./bin/oracle feed http://<rpc-host>:9650 <settlement-address> # + settlement gate
cd apps/settlement-feed/contracts && forge test               # 21 tests
```

Metrics on :9701 (`price`, `onchain_price`, `price_delta`, confirm latency);
delta 0 means the on-chain read-back matches the last published round, and it
is the staleness alarm.
