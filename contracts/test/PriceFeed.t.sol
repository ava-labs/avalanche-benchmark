// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

import {Test} from "forge-std/Test.sol";
import {PriceFeedAggregator} from "../src/PriceFeedAggregator.sol";
import {PriceFeedReceiver} from "../src/PriceFeedReceiver.sol";
import {
    IWarpMessenger,
    WarpMessage,
    WarpBlockHash
} from "../src/interfaces/IWarpMessenger.sol";

/// Minimal stand-in for the Warp precompile. Records the last payload sent and
/// serves a single configurable verified message, so tests can exercise the
/// real encode -> Warp -> decode round trip.
contract WarpMock is IWarpMessenger {
    bytes public lastPayload;
    WarpMessage internal verifiedMsg;
    bool internal verifiedOk;

    function sendWarpMessage(
        bytes calldata payload
    ) external returns (bytes32) {
        lastPayload = payload;
        return keccak256(payload);
    }

    function getVerifiedWarpMessage(
        uint32
    ) external view returns (WarpMessage memory, bool) {
        return (verifiedMsg, verifiedOk);
    }

    function getVerifiedWarpBlockHash(
        uint32
    ) external view returns (WarpBlockHash memory, bool) {
        WarpBlockHash memory b;
        return (b, false);
    }

    function getBlockchainID() external pure returns (bytes32) {
        return bytes32(0);
    }

    function setVerified(WarpMessage memory m, bool ok) external {
        verifiedMsg = m;
        verifiedOk = ok;
    }
}

contract PriceFeedTest is Test {
    address constant WARP_ADDR =
        0x0200000000000000000000000000000000000005;

    PriceFeedAggregator agg;
    PriceFeedReceiver rcv;
    WarpMock warp;

    address feeder = address(0xF33D);
    bytes32 sourceChainID = keccak256("oracle-l1");
    bytes32 constant ASSET = keccak256("AVAX/USD");

    function setUp() public {
        // Etch the mock at the precompile address.
        WarpMock impl = new WarpMock();
        vm.etch(WARP_ADDR, address(impl).code);
        warp = WarpMock(WARP_ADDR);

        // Both contracts have no constructor; seed their fixed slots the same
        // way genesis alloc storage would.
        agg = new PriceFeedAggregator();
        vm.store(address(agg), bytes32(uint256(0)), bytes32(uint256(uint160(feeder))));

        rcv = new PriceFeedReceiver();
        vm.store(address(rcv), bytes32(uint256(0)), sourceChainID);
        vm.store(
            address(rcv),
            bytes32(uint256(1)),
            bytes32(uint256(uint160(address(agg))))
        );
    }

    function test_feederAuth() public {
        assertEq(agg.feeder(), feeder);

        vm.expectRevert("not feeder");
        agg.submitPrice(ASSET, 100);

        vm.prank(feeder);
        agg.submitPrice(ASSET, 100);
        (uint256 price, uint256 updatedAt) = agg.latestPrice(ASSET);
        assertEq(price, 100);
        assertEq(updatedAt, block.timestamp);
    }

    function test_roundTrip() public {
        vm.warp(1_000);
        vm.prank(feeder);
        agg.submitPrice(ASSET, 4242);

        // Feed the aggregator's actual Warp payload back through the receiver.
        WarpMessage memory m = WarpMessage({
            sourceChainID: sourceChainID,
            originSenderAddress: address(agg),
            payload: warp.lastPayload()
        });
        warp.setVerified(m, true);

        rcv.receivePrice(0);
        (uint256 price, uint256 updatedAt) = rcv.latestPrice(ASSET);
        assertEq(price, 4242);
        assertEq(updatedAt, 1_000);
        // First submission carries seq 1.
        assertEq(agg.sequences(ASSET), 1);
    }

    function test_rejectInvalidMessage() public {
        WarpMessage memory m = WarpMessage({
            sourceChainID: sourceChainID,
            originSenderAddress: address(agg),
            payload: abi.encode(ASSET, uint256(1), uint256(1))
        });
        warp.setVerified(m, false);

        vm.expectRevert("invalid warp message");
        rcv.receivePrice(0);
    }

    function test_rejectWrongSourceChain() public {
        WarpMessage memory m = WarpMessage({
            sourceChainID: keccak256("some-other-chain"),
            originSenderAddress: address(agg),
            payload: abi.encode(ASSET, uint256(1), uint256(10))
        });
        warp.setVerified(m, true);

        vm.expectRevert("wrong source chain");
        rcv.receivePrice(0);
    }

    function test_rejectWrongOriginSender() public {
        WarpMessage memory m = WarpMessage({
            sourceChainID: sourceChainID,
            originSenderAddress: address(0xBAD),
            payload: abi.encode(ASSET, uint256(1), uint256(10))
        });
        warp.setVerified(m, true);

        vm.expectRevert("wrong origin sender");
        rcv.receivePrice(0);
    }

    function test_rejectOutOfOrderSeq() public {
        // Deliver seq 2 first.
        _deliver(500, 100, 2);
        (uint256 price, uint256 updatedAt) = rcv.latestPrice(ASSET);
        assertEq(price, 500);
        assertEq(updatedAt, 100);

        // Lower seq is rejected even with a newer timestamp.
        WarpMessage memory older = WarpMessage({
            sourceChainID: sourceChainID,
            originSenderAddress: address(agg),
            payload: abi.encode(ASSET, uint256(999), uint256(9_999), uint256(1))
        });
        warp.setVerified(older, true);
        vm.expectRevert("stale update");
        rcv.receivePrice(0);

        // Same seq is also rejected (replay).
        WarpMessage memory same = WarpMessage({
            sourceChainID: sourceChainID,
            originSenderAddress: address(agg),
            payload: abi.encode(ASSET, uint256(999), uint256(9_999), uint256(2))
        });
        warp.setVerified(same, true);
        vm.expectRevert("stale update");
        rcv.receivePrice(0);

        // Higher seq goes through.
        _deliver(600, 200, 3);
        (price, updatedAt) = rcv.latestPrice(ASSET);
        assertEq(price, 600);
        assertEq(updatedAt, 200);
    }

    // The whole point of the upgrade: two updates in the same second (identical
    // updatedAt) are both accepted as long as the sequence advances.
    function test_sameTimestampDifferentSeqAccepted() public {
        _deliver(100, 1_000, 1);
        _deliver(101, 1_000, 2);
        (uint256 price, uint256 updatedAt) = rcv.latestPrice(ASSET);
        assertEq(price, 101);
        assertEq(updatedAt, 1_000);
    }

    function _deliver(uint256 price, uint256 ts, uint256 seq) internal {
        WarpMessage memory m = WarpMessage({
            sourceChainID: sourceChainID,
            originSenderAddress: address(agg),
            payload: abi.encode(ASSET, price, ts, seq)
        });
        warp.setVerified(m, true);
        rcv.receivePrice(0);
    }
}
