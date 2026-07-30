// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

import {Test} from "forge-std/Test.sol";
import {PriceAggregator} from "../src/PriceAggregator.sol";
import {PriceFeedProxy} from "../src/PriceFeedProxy.sol";
import {UsdcSettlement} from "../src/examples/UsdcSettlement.sol";

contract UsdcSettlementTest is Test {
    // The consumer hardcodes the genesis proxy address, so the test etches
    // the proxy there instead of deploying at a fresh address.
    address constant PROXY_ADDR = 0x00000000000000000000000000000000FeedF00D;

    PriceAggregator agg;
    PriceFeedProxy proxy;
    UsdcSettlement settlement;

    address publisher = address(0xF33D);

    event Settled(
        address indexed party,
        uint256 amount,
        int256 price,
        uint80 roundId
    );

    function setUp() public {
        agg = new PriceAggregator();
        vm.store(address(agg), bytes32(uint256(0)), bytes32(uint256(uint160(publisher))));

        PriceFeedProxy impl = new PriceFeedProxy();
        vm.etch(PROXY_ADDR, address(impl).code);
        proxy = PriceFeedProxy(PROXY_ADDR);
        vm.store(PROXY_ADDR, bytes32(uint256(1)), bytes32(uint256(uint160(address(agg)))));
        vm.store(PROXY_ADDR, bytes32(uint256(2)), bytes32(uint256(1)));
        vm.store(PROXY_ADDR, keccak256(abi.encode(uint256(1), uint256(4))), bytes32(uint256(uint160(address(agg)))));

        settlement = new UsdcSettlement();
    }

    function test_settlesOnHealthyFeed() public {
        vm.warp(1_000);
        vm.prank(publisher);
        agg.submit(100_010_000); // $1.0001, in band

        vm.expectEmit(true, false, false, true);
        emit Settled(address(this), 500, 100_010_000, uint80((uint256(1) << 64) | 1));
        settlement.settle(500);
        assertEq(settlement.settled(), 500);

        (bool ok, string memory reason) = settlement.canSettle();
        assertTrue(ok);
        assertEq(reason, "");
    }

    function test_revertsOnDepeg() public {
        vm.warp(1_000);
        vm.startPrank(publisher);
        agg.submit(98_000_000); // $0.98, below band
        vm.stopPrank();

        vm.expectRevert("depegged");
        settlement.settle(500);

        (bool ok, string memory reason) = settlement.canSettle();
        assertFalse(ok);
        assertEq(reason, "depegged");

        // Above the band is refused too.
        vm.prank(publisher);
        agg.submit(102_000_000); // $1.02
        vm.expectRevert("depegged");
        settlement.settle(500);
    }

    function test_revertsOnStaleFeed() public {
        vm.warp(1_000);
        vm.prank(publisher);
        agg.submit(100_000_000);

        // Inside the freshness bound: fine.
        vm.warp(1_000 + 60);
        settlement.settle(1);

        // One second past it: refused.
        vm.warp(1_000 + 61);
        vm.expectRevert("stale price");
        settlement.settle(1);

        (bool ok, string memory reason) = settlement.canSettle();
        assertFalse(ok);
        assertEq(reason, "stale price");

        // The publisher resuming heals it.
        vm.prank(publisher);
        agg.submit(100_000_000);
        settlement.settle(1);
        assertEq(settlement.settled(), 2);
    }

    function test_revertsBeforeFirstRound() public {
        vm.expectRevert("No data present");
        settlement.settle(1);
    }
}
