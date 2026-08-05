# App: settlement-feed

This app is a Chainlink-compatible price feed with an example settlement
consumer. It runs on top of the base layer's L1. It is the first app in
the base-plus-apps layout. The app is self-contained. It does not depend
on any other app. Everything specific to this app is in this directory.

## Contents

- `contracts/`: the Solidity sources. `PriceAggregator` stores the rounds
  from one authorized publisher. `PriceFeedProxy` is the stable consumer
  address. It packs the phase into the round ids and swaps aggregators in
  two steps. Consumers read the `IPriceFeed` interface. Its signatures are
  identical to Chainlink's `AggregatorV3Interface`, so Chainlink consumers
  operate without changes. `Settlement.sol` is the example consumer. It
  permits settlement only when the price is in a band and is fresh.
- `cmd/oracle`: the off-chain services. `oracle feed <rpc-url>` publishes
  mock USDC-USD rounds as type-2 transactions and reads them back through
  the proxy. `oracle relay` is the Warp path for the optional oracle-L1
  shape.
- `oraclecontracts/`: the deployed bytecode, embedded for genesis baking.
  See the package comment. This import is the app's one edge into the
  base layer.
- `dashboards/`: the Direct Price Feed Grafana dashboard. Provision it
  together with the base dashboards.
- `docs/oracle-consumer.md`: the reference for teams that write consumers
  against the feed.

## Deployment

There are two supported paths:

1. Genesis-baked. This is the default. Every main-chain genesis contains
   the proxy at `0x...FeedF00D` and the aggregator at `0x...FEeDfAce`.
   `l1 create` bakes them, and `network.env` records the addresses. No
   runtime deployment is necessary.
2. After the fact. Deploy `Settlement`, or your own consumer, with forge
   against the running chain.

The oracle-L1 configuration files (`oracle-genesis-template.json`,
`subnet-config-oracle.json`) are at the repository root. The kit reads
them from the deployment root at run time. They belong to this app. They
move here when the base layer gets a configuration drop-in mechanism.

## Operation

```bash
./bin/oracle feed http://<rpc-host>:9650                      # publish rounds
./bin/oracle feed http://<rpc-host>:9650 <settlement-address> # also poll the settlement gate
cd apps/settlement-feed/contracts && forge test               # 21 tests
```

The feed exports metrics on port 9701: `price`, `onchain_price`,
`price_delta`, and the confirm latency. A delta of 0 means the on-chain
value is equal to the last published round. The delta is the staleness
alarm.
