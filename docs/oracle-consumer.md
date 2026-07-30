# Consuming oracle prices on-chain

The direct price feed is Chainlink-compatible: consumers read the kit's
`IPriceFeed` interface, signature-identical to Chainlink's
`AggregatorV3Interface`, from a proxy address, exactly as they would read a
Chainlink feed on mainnet. Code, libraries, and habits built against Chainlink
feeds work unchanged.

| What | Value |
|---|---|
| Consumer address (proxy) | `PriceFeedProxy` at `0x00000000000000000000000000000000FeedF00d` |
| Pair | USDC / USD (`description()` returns it) |
| Decimals | 8 (`decimals()` returns it): `100000000` = $1.00000000 |
| Writer | `PriceAggregator` at `0x00000000000000000000000000000000FeedFacE`, single authorized publisher |
| Sources | `contracts/src/PriceFeedProxy.sol`, `contracts/src/PriceAggregator.sol`, `contracts/src/interfaces/IPriceFeed.sol` |

Always point consumers at the proxy, never the aggregator. The proxy address
is permanent; the aggregator behind it can be swapped (mock feed to real feed,
or a new publisher model) without consumers noticing.

## Read the current price

Identical to the Chainlink consumer example:

```solidity
import {IPriceFeed} from "./interfaces/IPriceFeed.sol";

contract Consumer {
    IPriceFeed constant FEED =
        IPriceFeed(0x00000000000000000000000000000000FeedF00d);

    function usdcPrice() external view returns (int256 answer) {
        uint256 updatedAt;
        (, answer,, updatedAt,) = FEED.latestRoundData();
        require(answer > 0, "bad price");
        // Optional staleness bound. The demo feed updates ~10x per second,
        // so seconds of silence means the publisher is down.
        require(block.timestamp - updatedAt < 60, "stale price");
    }
}
```

`latestRoundData` reverts with `No data present` before the first submission,
the same behavior Chainlink aggregators have.

## Read historical values

Every submission is a round. `latestRoundData` returns the current `roundId`;
walk backwards with `getRoundData`:

```solidity
(uint80 roundId, int256 answer,, uint256 updatedAt,) = FEED.latestRoundData();
(, int256 previous,, uint256 previousAt,) = FEED.getRoundData(roundId - 1);
```

Round ids follow Chainlink's proxy convention: the upper bits carry a phase id
that increments when the aggregator behind the proxy is swapped, the lower 64
bits carry the aggregator's own round. History from an old phase stays
readable through its original round ids after a swap. For bulk history
(charting, analytics) index the aggregator's `AnswerUpdated` event instead of
calling `getRoundData` in a loop:

```solidity
event AnswerUpdated(int256 indexed current, uint256 indexed roundId, uint256 updatedAt);
```

## Example: a peg guard

`contracts/src/examples/UsdcSettlement.sol` is a complete consumer: a
settlement gate that proceeds only while USDC / USD is inside the $0.99 to
$1.01 band and the feed is at most 60 seconds old.

```solidity
(uint80 roundId, int256 price,, uint256 updatedAt,) = FEED.latestRoundData();
require(price >= MIN_PRICE && price <= MAX_PRICE, "depegged");
require(block.timestamp - updatedAt <= MAX_AGE, "stale price");
```

It also exposes the same checks as a non-reverting `canSettle()` view for UIs.
Both failure modes matter independently: "depegged" means the market moved,
"stale price" means the publisher stopped, and a consumer that checks only one
of them is unsafe against the other.

## From the command line

```bash
FEED=0x00000000000000000000000000000000FeedF00d
RPC=http://<rpc>:9650/ext/bc/<chain-id>/rpc

cast call $FEED "latestRoundData()(uint80,int256,uint256,uint256,uint80)" --rpc-url $RPC
cast call $FEED "decimals()(uint8)" --rpc-url $RPC
cast call $FEED "description()(string)" --rpc-url $RPC
cast call $FEED "getRoundData(uint80)(uint80,int256,uint256,uint256,uint80)" <roundId> --rpc-url $RPC
```

Divide `answer` by `1e8` for dollars.

## Choosing a freshness rule

- **`updatedAt`** is the wall-clock staleness bound, as in the `require`
  above. Second resolution, set by the block that mined the round.
- **`roundId`** is the ordering truth within a phase: it increments once per
  accepted update, so two updates inside the same second stay distinguishable.
- Updates land with a priority fee, so under chain congestion the price keeps
  updating at the front of each block; a stale price means the publisher
  itself stopped, not that it was priced out.

## Swapping the aggregator (operator note)

The proxy owner (the feeder key in this kit) runs Chainlink's two-step flow:
`proposeAggregator(next)` then `confirmAggregator(next)`. The phase id bumps,
consumers keep reading the same proxy address, and old-phase history stays
served by the old aggregator.

## Oracle-L1 deployments

A deployment that runs the optional oracle L1 delivers Warp-attested prices to
`PriceFeedReceiver` at `0x0000000000000000000000000000000000FeedED`
(`ORACLE_RECEIVER_ADDRESS` in `deployment/network.env`), which keeps its own
`latestPrice(bytes32 assetId)` read shape. The Chainlink-compatible direct
feed exists on main in both deployment shapes, so consumers written against
the proxy work unchanged either way.
