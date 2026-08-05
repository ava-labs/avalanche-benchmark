// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

import {Test} from "forge-std/Test.sol";
import {PriceAggregator} from "../src/PriceAggregator.sol";
import {PriceFeedProxy} from "../src/PriceFeedProxy.sol";
import {IPriceFeed} from "../src/interfaces/IPriceFeed.sol";

contract ChainlinkFeedTest is Test {
    PriceAggregator agg;
    PriceFeedProxy proxy;

    address publisher = address(0xF33D);
    address owner = address(0x0114E5);

    event AnswerUpdated(
        int256 indexed current,
        uint256 indexed roundId,
        uint256 updatedAt
    );

    // "USDC / USD" left-aligned with the byte length in the final byte, the
    // same value l1 create seeds into the aggregator's slot 1.
    bytes32 constant DESCRIPTION =
        bytes32(bytes("USDC / USD")) | bytes32(uint256(10));

    function setUp() public {
        // No constructors; seed the fixed slots the same way genesis alloc
        // storage would.
        agg = new PriceAggregator();
        vm.store(address(agg), bytes32(uint256(0)), bytes32(uint256(uint160(publisher))));
        vm.store(address(agg), bytes32(uint256(1)), DESCRIPTION);

        proxy = new PriceFeedProxy();
        _seedProxy(proxy, address(agg));
    }

    function _seedProxy(PriceFeedProxy target, address aggregatorAddress) internal {
        vm.store(address(target), bytes32(uint256(0)), bytes32(uint256(uint160(owner))));
        vm.store(address(target), bytes32(uint256(1)), bytes32(uint256(uint160(aggregatorAddress))));
        vm.store(address(target), bytes32(uint256(2)), bytes32(uint256(1)));
        // phaseAggregators[1] lives at keccak256(pad32(1) . pad32(4)).
        bytes32 slot = keccak256(abi.encode(uint256(1), uint256(4)));
        vm.store(address(target), slot, bytes32(uint256(uint160(aggregatorAddress))));
    }

    function test_publisherAuthAndEvent() public {
        vm.expectRevert("not publisher");
        agg.submit(1e8);

        vm.warp(1_000);
        vm.expectEmit(true, true, false, true);
        emit AnswerUpdated(1e8, 1, 1_000);
        vm.prank(publisher);
        agg.submit(1e8);
    }

    function test_metadata() public view {
        assertEq(agg.decimals(), 8);
        assertEq(agg.description(), "USDC / USD");
        assertEq(agg.version(), 1);
        // The proxy passes metadata through.
        assertEq(proxy.decimals(), 8);
        assertEq(proxy.description(), "USDC / USD");
        assertEq(proxy.version(), 1);
    }

    function test_noDataPresent() public {
        vm.expectRevert("No data present");
        agg.latestRoundData();

        vm.prank(publisher);
        agg.submit(1e8);

        vm.expectRevert("No data present");
        agg.getRoundData(2);

        // Unknown phase through the proxy.
        vm.expectRevert("No data present");
        proxy.getRoundData(uint80((uint256(9) << 64) | 1));
    }

    function test_roundsAndLatest() public {
        vm.startPrank(publisher);
        vm.warp(1_000);
        agg.submit(100_000_000);
        vm.warp(2_000);
        agg.submit(100_050_000);
        vm.stopPrank();

        (uint80 roundId, int256 answer, uint256 startedAt, uint256 updatedAt, uint80 answeredIn) =
            agg.latestRoundData();
        assertEq(roundId, 2);
        assertEq(answer, 100_050_000);
        assertEq(startedAt, 2_000);
        assertEq(updatedAt, 2_000);
        assertEq(answeredIn, 2);

        (, answer,, updatedAt,) = agg.getRoundData(1);
        assertEq(answer, 100_000_000);
        assertEq(updatedAt, 1_000);
    }

    // A consumer written against Chainlink docs, pointed at the proxy.
    function test_consumerThroughProxy() public {
        vm.warp(1_000);
        vm.prank(publisher);
        agg.submit(99_990_000);

        IPriceFeed feed = IPriceFeed(address(proxy));
        (uint80 roundId, int256 answer,, uint256 updatedAt, uint80 answeredIn) =
            feed.latestRoundData();

        // Phase 1 packed into the high bits.
        assertEq(roundId, uint80((uint256(1) << 64) | 1));
        assertEq(answeredIn, roundId);
        assertEq(answer, 99_990_000);
        assertEq(updatedAt, 1_000);

        // The packed round id reads back through getRoundData.
        (, int256 historical,,,) = feed.getRoundData(roundId);
        assertEq(historical, 99_990_000);
    }

    function test_aggregatorSwapKeepsHistory() public {
        vm.warp(1_000);
        vm.prank(publisher);
        agg.submit(100_000_000);
        (uint80 phase1Round,,,,) = proxy.latestRoundData();

        // Stand up the replacement aggregator (same genesis-shaped seeding).
        PriceAggregator next = new PriceAggregator();
        vm.store(address(next), bytes32(uint256(0)), bytes32(uint256(uint160(publisher))));
        vm.store(address(next), bytes32(uint256(1)), DESCRIPTION);
        vm.warp(2_000);
        vm.prank(publisher);
        next.submit(100_100_000);

        // Two-step swap, owner only.
        vm.expectRevert("not owner");
        proxy.proposeAggregator(address(next));
        vm.prank(owner);
        proxy.proposeAggregator(address(next));
        vm.prank(owner);
        vm.expectRevert("invalid proposed aggregator");
        proxy.confirmAggregator(address(0xBAD));
        vm.prank(owner);
        proxy.confirmAggregator(address(next));

        assertEq(proxy.aggregator(), address(next));
        assertEq(proxy.phaseId(), 2);

        // Latest reads come from phase 2.
        (uint80 roundId, int256 answer,,,) = proxy.latestRoundData();
        assertEq(roundId, uint80((uint256(2) << 64) | 1));
        assertEq(answer, 100_100_000);

        // Phase-1 history still resolves through its packed round id.
        (, int256 historical,, uint256 updatedAt,) = proxy.getRoundData(phase1Round);
        assertEq(historical, 100_000_000);
        assertEq(updatedAt, 1_000);
    }
}
