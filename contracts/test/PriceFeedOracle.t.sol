// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

import {Test} from "forge-std/Test.sol";
import {PriceFeedOracle} from "../src/PriceFeedOracle.sol";

contract PriceFeedOracleTest is Test {
    PriceFeedOracle oracle;

    address feeder = address(0xF33D);
    bytes32 constant ASSET = keccak256("USDC-USD");

    event PriceUpdated(
        bytes32 indexed assetId,
        uint256 price,
        uint256 updatedAt,
        uint256 seq
    );

    function setUp() public {
        // No constructor; seed the fixed feeder slot the same way genesis
        // alloc storage would.
        oracle = new PriceFeedOracle();
        vm.store(
            address(oracle),
            bytes32(uint256(0)),
            bytes32(uint256(uint160(feeder)))
        );
    }

    function test_feederAuth() public {
        assertEq(oracle.feeder(), feeder);

        vm.expectRevert("not feeder");
        oracle.submitPrice(ASSET, 100);

        vm.warp(1_000);
        vm.expectEmit(true, false, false, true);
        emit PriceUpdated(ASSET, 100, 1_000, 1);
        vm.prank(feeder);
        oracle.submitPrice(ASSET, 100);

        (uint256 price, uint256 updatedAt) = oracle.latestPrice(ASSET);
        assertEq(price, 100);
        assertEq(updatedAt, 1_000);
        assertEq(oracle.sequences(ASSET), 1);
    }

    function test_sameTimestampUpdatesOrderedBySeq() public {
        vm.warp(1_000);
        vm.startPrank(feeder);
        oracle.submitPrice(ASSET, 100);
        oracle.submitPrice(ASSET, 101);
        vm.stopPrank();

        (uint256 price, uint256 updatedAt, uint256 seq) = oracle.latestRound(
            ASSET
        );
        assertEq(price, 101);
        assertEq(updatedAt, 1_000);
        assertEq(seq, 2);
    }

    function test_historicalRounds() public {
        vm.startPrank(feeder);
        vm.warp(1_000);
        oracle.submitPrice(ASSET, 100);
        vm.warp(2_000);
        oracle.submitPrice(ASSET, 200);
        vm.warp(3_000);
        oracle.submitPrice(ASSET, 300);
        vm.stopPrank();

        (uint256 price, uint256 updatedAt) = oracle.priceAt(ASSET, 1);
        assertEq(price, 100);
        assertEq(updatedAt, 1_000);

        (price, updatedAt) = oracle.priceAt(ASSET, 2);
        assertEq(price, 200);
        assertEq(updatedAt, 2_000);

        (price, updatedAt) = oracle.priceAt(ASSET, 3);
        assertEq(price, 300);
        assertEq(updatedAt, 3_000);

        // The latest round and latestPrice agree.
        (price, updatedAt) = oracle.latestPrice(ASSET);
        assertEq(price, 300);
        assertEq(updatedAt, 3_000);
    }

    function test_unknownRoundReverts() public {
        vm.prank(feeder);
        oracle.submitPrice(ASSET, 100);

        vm.expectRevert("unknown round");
        oracle.priceAt(ASSET, 0);

        vm.expectRevert("unknown round");
        oracle.priceAt(ASSET, 2);

        vm.expectRevert("unknown round");
        oracle.priceAt(keccak256("BTC-USD"), 1);
    }

    function test_assetsIsolated() public {
        bytes32 other = keccak256("BTC-USD");
        vm.startPrank(feeder);
        oracle.submitPrice(ASSET, 100);
        oracle.submitPrice(other, 60_000);
        vm.stopPrank();

        assertEq(oracle.sequences(ASSET), 1);
        assertEq(oracle.sequences(other), 1);
        (uint256 price, ) = oracle.latestPrice(ASSET);
        assertEq(price, 100);
        (price, ) = oracle.latestPrice(other);
        assertEq(price, 60_000);
    }
}
