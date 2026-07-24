// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

import {IWarpMessenger, WarpMessage} from "./interfaces/IWarpMessenger.sol";

/// @title PriceFeedReceiver
/// @notice Deployed on the main L1. Ingests Warp messages produced by the
///         PriceFeedAggregator on the oracle L1 and stores the latest price
///         per asset.
/// @dev No constructor. Config lives in fixed storage slots so the runtime
///      bytecode can be baked into genesis alloc and its storage seeded there:
///        slot 0 = expected source blockchain ID (bytes32)
///        slot 1 = expected origin sender (aggregator address)
///      Adding a constructor or immutables would move config into bytecode and
///      break genesis-alloc deployment.
contract PriceFeedReceiver {
    IWarpMessenger constant WARP =
        IWarpMessenger(0x0200000000000000000000000000000000000005);

    struct Price {
        uint256 price;
        uint256 updatedAt;
        uint256 seq;
    }

    // slot 0: expected source chain. Seeded via genesis alloc storage.
    bytes32 public sourceChainID;

    // slot 1: expected origin sender (the aggregator). Seeded via genesis alloc.
    address public originSender;

    // slot 2: latest price per asset.
    mapping(bytes32 => Price) internal prices;

    event PriceReceived(
        bytes32 indexed assetId,
        uint256 price,
        uint256 updatedAt
    );

    function receivePrice(uint32 messageIndex) external {
        (WarpMessage memory message, bool valid) = WARP.getVerifiedWarpMessage(
            messageIndex
        );
        require(valid, "invalid warp message");
        require(message.sourceChainID == sourceChainID, "wrong source chain");
        require(
            message.originSenderAddress == originSender,
            "wrong origin sender"
        );

        // Payload is EXACTLY four 32-byte words: assetId, price, updatedAt, seq.
        (bytes32 assetId, uint256 price, uint256 updatedAt, uint256 seq) = abi
            .decode(message.payload, (bytes32, uint256, uint256, uint256));

        // Freshness is by sequence, not timestamp, so multiple updates per
        // second per asset are accepted. updatedAt is kept only for display.
        require(seq > prices[assetId].seq, "stale update");

        prices[assetId] = Price(price, updatedAt, seq);
        emit PriceReceived(assetId, price, updatedAt);
    }

    function latestPrice(
        bytes32 assetId
    ) external view returns (uint256 price, uint256 updatedAt) {
        Price storage p = prices[assetId];
        return (p.price, p.updatedAt);
    }
}
