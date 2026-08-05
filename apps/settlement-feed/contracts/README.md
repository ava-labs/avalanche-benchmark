# Price feed contracts

The Solidity sources for the settlement-feed app.

- `src/PriceAggregator.sol`: deployed on the **main L1** in every
  deployment shape, one instance per pair. It is the Chainlink-shaped
  writer for the direct feed. One authorized publisher calls
  `submit(int256)`. Every round is stored. Reads are `IPriceFeed`.
- `src/PriceFeedProxy.sol`: deployed on the **main L1** in front of the
  aggregator. It is shaped after Chainlink's EACAggregatorProxy: the
  stable consumer address, phase-packed round ids, and owner-gated
  two-step aggregator swaps. See `../docs/oracle-consumer.md`.
- `src/examples/Settlement.sol`: the example consumer. It is a peg-guard
  settlement gate over the proxy, with a band check and a staleness check.
- `src/interfaces/IPriceFeed.sol`: the consumer read interface. Its
  signatures are identical to Chainlink's `AggregatorV3Interface`, so the
  selectors match exactly. The file is original; only the ABI shape is
  shared.
- `src/PriceFeedAggregator.sol`: deployed on the optional **oracle L1**.
  An authorized feeder submits prices. Each submission emits one Warp
  message through the Warp precompile. The payload is exactly four
  32-byte words, in this order: `abi.encode(assetId, price, updatedAt,
  seq)`. The Go relay parses them by position. `seq` is a monotonic
  per-asset counter, so freshness is sequence-based, and more than one
  update per second per asset is possible.
- `src/PriceFeedReceiver.sol`: deployed on the **main L1** for the
  oracle-L1 shape. It ingests verified Warp messages from the aggregator
  and stores the latest price per asset. It rejects out-of-order and
  replayed updates, and messages from the wrong source chain or origin
  sender.
- `src/interfaces/IWarpMessenger.sol`: vendored verbatim from
  `ava-labs/subnet-evm`
  (`precompile/contracts/warp/warpbindings/IWarpMessenger.sol`). The
  precompile is at `0x0200000000000000000000000000000000000005`.

## No constructors: configuration lives in fixed storage slots

The contracts have no constructors and no immutables. Immutables are baked
into bytecode, and constructor logic never runs when runtime bytecode goes
directly into a genesis `alloc`. All configuration is therefore read from
fixed storage slots. Genesis alloc `storage` entries seed them:

| Contract              | slot 0                         | slot 1                       |
| --------------------- | ------------------------------ | ---------------------------- |
| `PriceFeedAggregator` | authorized feeder address      | (prices mapping base)        |
| `PriceFeedReceiver`   | expected source blockchain ID  | expected origin sender addr  |
| `PriceAggregator`     | authorized publisher address   | short-string description     |
| `PriceFeedProxy`      | owner                          | current aggregator address   |

`PriceFeedProxy` additionally seeds slot 2 (phase id 1) and the
`phaseAggregators[1]` mapping entry at `keccak256(pad32(1) . pad32(4))`.
`PriceFeedAggregator` slot 2 is the per-asset `sequences` mapping base,
and `PriceFeedReceiver` slot 2 is its prices mapping base. Genesis seeding
only touches the fixed scalar slots in the table.

## Artifacts consumed by the Go code

The artifacts are checked in, so the Go build never needs `solc` or
`forge`:

- `artifacts/PriceFeedAggregator.runtime.hex`: the deployed (runtime)
  bytecode, one `0x`-prefixed line, baked into the oracle chain's genesis
  alloc.
- `artifacts/PriceFeedReceiver.runtime.hex`: the same, for the main L1.
- `artifacts/PriceAggregator.runtime.hex` and
  `artifacts/PriceFeedProxy.runtime.hex`: the same, for the main L1's
  direct feed.
- `artifacts/selectors.json`: the 4-byte selectors for `submitPrice`,
  `receivePrice`, `receivePrices` (batched, up to 32 per call),
  `latestPrice`, `submit`, `latestRoundData`, `getRoundData`.

## Regenerate

The Go code embeds copies from the sibling `../oraclecontracts/` package
(`l1 create` bakes them into genesis). A regeneration must update BOTH
locations:

```sh
forge install foundry-rs/forge-std   # lib/ is not committed
forge build
forge test -vv
forge inspect src/PriceFeedAggregator.sol:PriceFeedAggregator deployedBytecode > artifacts/PriceFeedAggregator.runtime.hex
forge inspect src/PriceFeedReceiver.sol:PriceFeedReceiver deployedBytecode > artifacts/PriceFeedReceiver.runtime.hex
forge inspect src/PriceAggregator.sol:PriceAggregator deployedBytecode > artifacts/PriceAggregator.runtime.hex
forge inspect src/PriceFeedProxy.sol:PriceFeedProxy deployedBytecode > artifacts/PriceFeedProxy.runtime.hex
cp artifacts/*.runtime.hex artifacts/selectors.json ../oraclecontracts/
```

Selectors come from `cast sig`, for example
`cast sig "submitPrice(bytes32,uint256)"`.
