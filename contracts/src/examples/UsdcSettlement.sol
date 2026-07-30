// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

import {IPriceFeed} from "../interfaces/IPriceFeed.sol";

/// @title UsdcSettlement
/// @notice Example consumer: a settlement gate that only proceeds while the
///         USDC / USD feed is healthy. "Healthy" is two independent checks a
///         consumer should always make:
///           1. the price is inside the accepted peg band, and
///           2. the price is fresh (the publisher has not stopped).
///         Settlement itself is symbolic here (an event and a counter); in a
///         real system this wrapper sits in front of the transfer, mint, or
///         netting step.
/// @dev The feed proxy lives at a fixed genesis address, so the reference can
///      be a constant: nothing to configure, nothing to deploy but this file.
contract UsdcSettlement {
    IPriceFeed public constant FEED =
        IPriceFeed(0x00000000000000000000000000000000FeedF00D);

    // Accepted peg band and freshness bound, 8-decimal fixed point / seconds.
    int256 public constant MIN_PRICE = 99_000_000; // $0.99
    int256 public constant MAX_PRICE = 101_000_000; // $1.01
    uint256 public constant MAX_AGE = 60; // seconds

    uint256 public settled;

    event Settled(
        address indexed party,
        uint256 amount,
        int256 price,
        uint80 roundId
    );

    /// @notice Gate a settlement of `amount` (USDC base units) on feed health.
    function settle(uint256 amount) external {
        (uint80 roundId, int256 price,, uint256 updatedAt,) =
            FEED.latestRoundData();

        require(price >= MIN_PRICE && price <= MAX_PRICE, "depegged");
        require(block.timestamp - updatedAt <= MAX_AGE, "stale price");

        settled += amount;
        emit Settled(msg.sender, amount, price, roundId);
    }

    /// @notice The same checks as a view, for UIs and pre-flight checks.
    ///         Returns ok=false with a reason instead of reverting. A missing
    ///         first round still reverts (the feed proxy reverts "No data
    ///         present"), which is correct: an unpublished feed is not a
    ///         "temporarily unhealthy" state a UI should retry through.
    function canSettle() external view returns (bool ok, string memory reason) {
        (, int256 price,, uint256 updatedAt,) = FEED.latestRoundData();
        if (price < MIN_PRICE || price > MAX_PRICE) {
            return (false, "depegged");
        }
        if (block.timestamp - updatedAt > MAX_AGE) {
            return (false, "stale price");
        }
        return (true, "");
    }
}
