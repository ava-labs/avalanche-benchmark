// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

import {IWarpMessenger} from "./interfaces/IWarpMessenger.sol";

/// @title PriceFeedAggregator
/// @notice Deployed on the oracle L1. A single authorized feeder submits prices;
///         each submission is broadcast as one Warp message to the main L1.
/// @dev No constructor. Config lives in fixed storage slots so the runtime
///      bytecode can be baked into genesis alloc and its storage seeded there:
///        slot 0 = authorized feeder address.
///      Adding a constructor or immutables would move config into bytecode and
///      break genesis-alloc deployment.
contract PriceFeedAggregator {
    IWarpMessenger constant WARP =
        IWarpMessenger(0x0200000000000000000000000000000000000005);

    struct Price {
        uint256 price;
        uint256 updatedAt;
    }

    // slot 0: authorized feeder. Seeded via genesis alloc storage.
    address public feeder;

    // slot 1: latest price per asset.
    mapping(bytes32 => Price) internal prices;

    // slot 2: monotonic per-asset sequence. Freshness on the receiver is by seq,
    // not timestamp, so more than one update per second per asset is possible.
    mapping(bytes32 => uint256) public sequences;

    event PriceSubmitted(
        bytes32 indexed assetId,
        uint256 price,
        uint256 updatedAt,
        uint256 seq,
        bytes32 warpMessageID
    );

    function submitPrice(bytes32 assetId, uint256 price) external {
        require(msg.sender == feeder, "not feeder");

        uint256 updatedAt = block.timestamp;
        uint256 seq = ++sequences[assetId];
        prices[assetId] = Price(price, updatedAt);

        // Payload is EXACTLY four 32-byte words in this order; the Go relay
        // parses them positionally: assetId, price, updatedAt, seq.
        bytes32 warpMessageID = WARP.sendWarpMessage(
            abi.encode(assetId, price, updatedAt, seq)
        );

        emit PriceSubmitted(assetId, price, updatedAt, seq, warpMessageID);
    }

    function latestPrice(
        bytes32 assetId
    ) external view returns (uint256 price, uint256 updatedAt) {
        Price storage p = prices[assetId];
        return (p.price, p.updatedAt);
    }
}
