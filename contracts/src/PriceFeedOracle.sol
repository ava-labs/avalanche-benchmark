// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

/// @title PriceFeedOracle
/// @notice Deployed on the main L1. The direct-publish variant of the price
///         feed: a single authorized feeder pushes prices straight to this
///         contract (no oracle chain, no Warp hop) and consumers read the
///         latest value or any historical round. submitPrice/latestPrice keep
///         the exact ABI of PriceFeedAggregator so the same feeder tooling
///         drives either deployment shape.
/// @dev No constructor. Config lives in a fixed storage slot so the runtime
///      bytecode can be baked into genesis alloc and its storage seeded there:
///        slot 0 = authorized feeder address.
///      Adding a constructor or immutables would move config into bytecode and
///      break genesis-alloc deployment.
contract PriceFeedOracle {
    struct Price {
        uint256 price;
        uint256 updatedAt;
    }

    // slot 0: authorized feeder. Seeded via genesis alloc storage.
    address public feeder;

    // slot 1: latest price per asset.
    mapping(bytes32 => Price) internal prices;

    // slot 2: monotonic per-asset sequence. Freshness is by seq, not
    // timestamp, so more than one update per second per asset is ordered.
    mapping(bytes32 => uint256) public sequences;

    // slot 3: every submission per asset, keyed by seq (1-based), so
    // consumers can read historical values on-chain.
    mapping(bytes32 => mapping(uint256 => Price)) internal rounds;

    event PriceUpdated(
        bytes32 indexed assetId,
        uint256 price,
        uint256 updatedAt,
        uint256 seq
    );

    function submitPrice(bytes32 assetId, uint256 price) external {
        require(msg.sender == feeder, "not feeder");

        uint256 updatedAt = block.timestamp;
        uint256 seq = ++sequences[assetId];
        Price memory update = Price(price, updatedAt);
        prices[assetId] = update;
        rounds[assetId][seq] = update;

        emit PriceUpdated(assetId, price, updatedAt, seq);
    }

    function latestPrice(
        bytes32 assetId
    ) external view returns (uint256 price, uint256 updatedAt) {
        Price storage p = prices[assetId];
        return (p.price, p.updatedAt);
    }

    function latestRound(
        bytes32 assetId
    ) external view returns (uint256 price, uint256 updatedAt, uint256 seq) {
        Price storage p = prices[assetId];
        return (p.price, p.updatedAt, sequences[assetId]);
    }

    function priceAt(
        bytes32 assetId,
        uint256 seq
    ) external view returns (uint256 price, uint256 updatedAt) {
        require(seq >= 1 && seq <= sequences[assetId], "unknown round");
        Price storage p = rounds[assetId][seq];
        return (p.price, p.updatedAt);
    }
}
