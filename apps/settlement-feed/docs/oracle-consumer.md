# Consume oracle prices on-chain

The direct price feed is Chainlink-compatible. Consumers read the
`IPriceFeed` interface from a proxy address. The interface signatures are
identical to Chainlink's `AggregatorV3Interface`. Code and libraries
written against Chainlink feeds work without changes.

| What | Value |
|---|---|
| Consumer address (proxy) | `PriceFeedProxy` at `0x00000000000000000000000000000000FeedF00d` |
| Pair | USDC / USD (`description()` returns it) |
| Decimals | 8 (`decimals()` returns it): `100000000` = $1.00000000 |
| Writer | `PriceAggregator` at `0x00000000000000000000000000000000FeedFacE`, one authorized publisher |
| Sources | `contracts/src/PriceFeedProxy.sol`, `contracts/src/PriceAggregator.sol`, `contracts/src/interfaces/IPriceFeed.sol` |

Point consumers at the proxy, never at the aggregator. The proxy address
is permanent. The operator can swap the aggregator behind it, for example
from the mock feed to a real feed, and consumers see no change.

## Read the current price

This is identical to the Chainlink consumer example:

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

Before the first submission, `latestRoundData` reverts with
`No data present`. Chainlink aggregators have the same behavior.

## Read historical values

Every submission is a round. `latestRoundData` returns the current
`roundId`. Walk backwards with `getRoundData`:

```solidity
(uint80 roundId, int256 answer,, uint256 updatedAt,) = FEED.latestRoundData();
(, int256 previous,, uint256 previousAt,) = FEED.getRoundData(roundId - 1);
```

Round ids follow Chainlink's proxy convention. The upper bits carry a
phase id. The phase id increments when the operator swaps the aggregator.
The lower 64 bits carry the aggregator's own round number. History from an
old phase stays readable through its original round ids after a swap.

For bulk history, for example charts or analytics, index the aggregator's
`AnswerUpdated` event. Do not call `getRoundData` in a loop.

```solidity
event AnswerUpdated(int256 indexed current, uint256 indexed roundId, uint256 updatedAt);
```

## Example: a peg guard

`contracts/src/examples/Settlement.sol` is a complete consumer. It is a
settlement gate. It proceeds only while USDC/USD is inside the $0.99 to
$1.01 band and the feed is at most 60 seconds old.

```solidity
(uint80 roundId, int256 price,, uint256 updatedAt,) = FEED.latestRoundData();
require(price >= MIN_PRICE && price <= MAX_PRICE, "depegged");
require(block.timestamp - updatedAt <= MAX_AGE, "stale price");
```

It also exposes the same checks as a `canSettle()` view that does not
revert, for UIs. The two failure modes are independent. "depegged" means
the market moved. "stale price" means the publisher stopped. A consumer
that checks only one of them is not safe against the other.

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

## Select a freshness rule

- `updatedAt` is the wall-clock staleness bound, as in the `require`
  above. It has second resolution. The block that mined the round sets it.
- `roundId` is the ordering truth inside a phase. It increments once per
  accepted update. Two updates inside the same second stay
  distinguishable.
- Updates pay a priority fee. Under chain congestion, the price keeps
  updating at the front of each block. A stale price therefore means the
  publisher stopped, not that congestion priced it out.

## Swap the aggregator (operator note)

The proxy owner is the feeder key in this kit. The owner runs Chainlink's
two-step flow: `proposeAggregator(next)`, then `confirmAggregator(next)`.
The phase id increments. Consumers keep reading the same proxy address.
The old aggregator keeps serving the old phase's history.

## Oracle-L1 deployments

A deployment with the optional oracle L1 also delivers Warp-attested
prices to `PriceFeedReceiver` at
`0x0000000000000000000000000000000000FeedED`
(`ORACLE_RECEIVER_ADDRESS` in `deployment/network.env`). The receiver has
its own `latestPrice(bytes32 assetId)` read shape. The Chainlink-compatible
direct feed exists on the main chain in both deployment shapes, so
consumers written against the proxy work without changes in both.
