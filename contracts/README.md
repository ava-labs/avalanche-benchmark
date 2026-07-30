# Oracle Warp contracts

Two minimal Solidity contracts for the subnet-evm price-oracle demo.

- **`src/PriceFeedAggregator.sol`** — deployed on the **oracle L1**. An authorized
  feeder submits prices; each submission emits one Warp message via the Warp
  precompile. The payload is **exactly four 32-byte words in this order**:
  `abi.encode(assetId, price, updatedAt, seq)` — the Go relay parses them
  positionally. `seq` is a monotonic per-asset counter, so freshness is
  sequence-based (not timestamp-based) and more than one update per second per
  asset is possible.
- **`src/PriceFeedReceiver.sol`** — deployed on the **main L1**. Ingests verified
  Warp messages from the aggregator and stores the latest price per asset,
  rejecting stale (out-of-order/replayed) updates and messages from the wrong
  source chain or origin sender.
- **`src/PriceFeedOracle.sol`** — deployed on the **main L1** in every
  deployment shape. The direct-publish variant: same `submitPrice`/`latestPrice`
  ABI as the aggregator, no Warp broadcast, plus per-`seq` round history
  (`latestRound`, `priceAt`) so consumers can read historical values on-chain.
  See `../docs/oracle-consumer.md`.
- **`src/interfaces/IWarpMessenger.sol`** — vendored verbatim from
  `ava-labs/subnet-evm` (`precompile/contracts/warp/warpbindings/IWarpMessenger.sol`).
  The precompile lives at `0x0200000000000000000000000000000000000005`.

## No constructors — config lives in fixed storage slots

Both contracts have **no constructor and no immutables**. Immutables are baked
into bytecode, and constructor logic never runs when runtime bytecode is placed
directly into a genesis `alloc`. So all config is read from fixed storage slots,
which are seeded via genesis alloc `storage` entries:

| Contract              | slot 0                         | slot 1                       |
| --------------------- | ------------------------------ | ---------------------------- |
| `PriceFeedAggregator` | authorized feeder address      | (prices mapping base)        |
| `PriceFeedReceiver`   | expected source blockchain ID  | expected origin sender addr  |
| `PriceFeedOracle`     | authorized feeder address      | (prices mapping base)        |

(`PriceFeedAggregator` slot 2 is the per-asset `sequences` mapping base;
`PriceFeedReceiver` slot 2 is its prices mapping base. Genesis seeding only
touches the fixed scalar slots above.)

## Artifacts consumed by `cmd/oracle`

Checked in so the Go build never needs `solc`/`forge`:

- `artifacts/PriceFeedAggregator.runtime.hex` — deployed (runtime) bytecode, one
  `0x`-prefixed line. Bake into the oracle chain's genesis alloc.
- `artifacts/PriceFeedReceiver.runtime.hex` — same, for the main L1.
- `artifacts/PriceFeedOracle.runtime.hex` — same, for the main L1's
  direct-publish contract.
- `artifacts/selectors.json` — 4-byte selectors for `submitPrice`,
  `receivePrice`, `receivePrices` (batched, up to 32/call), `latestPrice`,
  `latestRound`, `priceAt`.

## Regenerate

The Go code embeds copies from `internal/oraclecontracts/` (`l1 create` bakes
them into Genesis), so regeneration must update BOTH locations:

```sh
forge install foundry-rs/forge-std   # lib/ is not committed
forge build
forge test -vv
forge inspect src/PriceFeedAggregator.sol:PriceFeedAggregator deployedBytecode > artifacts/PriceFeedAggregator.runtime.hex
forge inspect src/PriceFeedReceiver.sol:PriceFeedReceiver deployedBytecode > artifacts/PriceFeedReceiver.runtime.hex
forge inspect src/PriceFeedOracle.sol:PriceFeedOracle deployedBytecode > artifacts/PriceFeedOracle.runtime.hex
cp artifacts/*.runtime.hex artifacts/selectors.json ../internal/oraclecontracts/
```

Selectors: `cast sig "submitPrice(bytes32,uint256)"` etc.
