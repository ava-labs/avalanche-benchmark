# Consuming oracle prices on-chain

How a contract (or an off-chain reader) consumes prices published to the
`PriceFeedOracle` on the main chain. This covers the direct-publish
deployment; the address below is fixed in genesis on every deployment.

| What | Value |
|---|---|
| Contract | `PriceFeedOracle` at `0x00000000000000000000000000000000FeedF00d` |
| Asset key | `assetId = keccak256("USDC-USD")` (bytes32; one key per pair) |
| Price scale | 8-decimal fixed point: `100000000` = $1.00000000 |
| Freshness | `seq` is a per-asset monotonic counter (1-based); `updatedAt` is the block timestamp, second resolution, kept for display |
| Source | `contracts/src/PriceFeedOracle.sol` |

## Read the current price

```solidity
interface IPriceFeedOracle {
    function latestPrice(bytes32 assetId)
        external view returns (uint256 price, uint256 updatedAt);
    function latestRound(bytes32 assetId)
        external view returns (uint256 price, uint256 updatedAt, uint256 seq);
    function priceAt(bytes32 assetId, uint256 seq)
        external view returns (uint256 price, uint256 updatedAt);
}

contract Consumer {
    IPriceFeedOracle constant ORACLE =
        IPriceFeedOracle(0x00000000000000000000000000000000FeedF00d);
    bytes32 constant USDC_USD = keccak256("USDC-USD");

    function usdcPrice() external view returns (uint256 price) {
        uint256 updatedAt;
        (price, updatedAt) = ORACLE.latestPrice(USDC_USD);
        require(price != 0, "no price yet");
        // Optional staleness bound. The demo feed updates ~10x per second,
        // so seconds of silence means the publisher is down.
        require(block.timestamp - updatedAt < 60, "stale price");
    }
}
```

`latestPrice` returns `(0, 0)` for an asset that has never been published;
check for zero before dividing by it.

## Read historical values

Every submission is stored as a round keyed by its sequence number, so history
is an on-chain read, not an event scan:

```solidity
(uint256 price, uint256 updatedAt, uint256 seq) = ORACLE.latestRound(USDC_USD);
// Walk backwards: rounds seq, seq-1, ..., 1.
(uint256 previousPrice, uint256 previousAt) = ORACLE.priceAt(USDC_USD, seq - 1);
```

`priceAt` reverts with `unknown round` outside `[1, latest seq]`. For bulk
history (charting, analytics) prefer indexing the event instead of many
`priceAt` calls:

```solidity
event PriceUpdated(bytes32 indexed assetId, uint256 price, uint256 updatedAt, uint256 seq);
```

## From the command line

```bash
ORACLE=0x00000000000000000000000000000000FeedF00d
RPC=http://<rpc>:9650/ext/bc/<chain-id>/rpc
ASSET=$(cast keccak "USDC-USD")

cast call $ORACLE "latestPrice(bytes32)(uint256,uint256)" $ASSET --rpc-url $RPC
cast call $ORACLE "latestRound(bytes32)(uint256,uint256,uint256)" $ASSET --rpc-url $RPC
cast call $ORACLE "priceAt(bytes32,uint256)(uint256,uint256)" $ASSET 1 --rpc-url $RPC
```

Divide the returned price by `1e8` for dollars.

## Choosing a freshness rule

- **`seq`** is the ordering truth: it increments once per accepted update, so
  two updates inside the same second are still distinguishable. Compare `seq`
  when you need "is this newer than what I last used".
- **`updatedAt`** is for wall-clock staleness bounds like the `require` above.
  It has second resolution and is set by the block that mined the update.
- Updates land with a priority fee, so under chain congestion the price keeps
  updating at the front of each block; a stale price means the publisher
  itself stopped, not that it was priced out.

## Oracle-L1 deployments

A deployment that runs the optional oracle L1 delivers Warp-attested prices to
a different contract, `PriceFeedReceiver` at
`0x0000000000000000000000000000000000FeedED` (`ORACLE_RECEIVER_ADDRESS` in
`deployment/network.env`). Its read side is the same shape: `latestPrice`
returns `(price, updatedAt)` with the same 8-decimal scale, and freshness is
the same per-asset `seq`. The direct `PriceFeedOracle` exists on main in both
shapes, so consumers written against it work unchanged either way.
