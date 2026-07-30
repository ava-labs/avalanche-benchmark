// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

import {IPriceFeed} from "./interfaces/IPriceFeed.sol";

/// @title PriceFeedProxy
/// @notice The stable per-pair address consumers point at, shaped after
///         Chainlink's EACAggregatorProxy: the owner can swap the aggregator
///         behind it (a new "phase") without consumers changing anything.
///         Round ids returned to consumers pack the phase in the high bits,
///         so historical reads keep working across aggregator swaps.
/// @dev No constructor. Config lives in fixed storage slots so the runtime
///      bytecode can be baked into genesis alloc and its storage seeded there:
///        slot 0 = owner
///        slot 1 = current aggregator address
///        slot 2 = current phase id (seeded to 1)
///        slot 4 = phaseAggregators mapping base; genesis also seeds the
///                 phase-1 entry (keccak256(pad32(1) . pad32(4)))
///      Adding a constructor or immutables would move config into bytecode and
///      break genesis-alloc deployment.
contract PriceFeedProxy is IPriceFeed {
    uint256 private constant PHASE_OFFSET = 64;

    // slot 0: owner (may propose/confirm aggregator swaps). Genesis-seeded.
    address public owner;

    // slot 1: the aggregator answering reads right now. Genesis-seeded.
    address private currentAggregator;

    // slot 2: current phase, bumped on every confirmed swap. Genesis-seeded to 1.
    uint256 private currentPhaseId;

    // slot 3: two-step swap staging.
    address public proposedAggregator;

    // slot 4: every phase's aggregator, for historical getRoundData.
    // Genesis seeds phase 1.
    mapping(uint16 => address) public phaseAggregators;

    event AggregatorProposed(address indexed current, address indexed proposed);
    event AggregatorConfirmed(address indexed previous, address indexed latest);

    modifier onlyOwner() {
        require(msg.sender == owner, "not owner");
        _;
    }

    function aggregator() external view returns (address) {
        return currentAggregator;
    }

    function phaseId() external view returns (uint16) {
        return uint16(currentPhaseId);
    }

    /// @notice Two-step swap, exactly Chainlink's propose/confirm flow: a fat
    ///         finger proposes a wrong address, confirm refuses it.
    function proposeAggregator(address next) external onlyOwner {
        proposedAggregator = next;
        emit AggregatorProposed(currentAggregator, next);
    }

    function confirmAggregator(address next) external onlyOwner {
        require(next == proposedAggregator, "invalid proposed aggregator");
        delete proposedAggregator;
        address previous = currentAggregator;
        currentAggregator = next;
        uint16 phase = uint16(++currentPhaseId);
        phaseAggregators[phase] = next;
        emit AggregatorConfirmed(previous, next);
    }

    function decimals() external view returns (uint8) {
        return IPriceFeed(currentAggregator).decimals();
    }

    function description() external view returns (string memory) {
        return IPriceFeed(currentAggregator).description();
    }

    function version() external view returns (uint256) {
        return IPriceFeed(currentAggregator).version();
    }

    function getRoundData(
        uint80 _roundId
    )
        external
        view
        returns (
            uint80 roundId,
            int256 answer,
            uint256 startedAt,
            uint256 updatedAt,
            uint80 answeredInRound
        )
    {
        uint16 phase = uint16(_roundId >> PHASE_OFFSET);
        address phaseAggregator = phaseAggregators[phase];
        require(phaseAggregator != address(0), "No data present");
        (
            uint80 innerRound,
            int256 innerAnswer,
            uint256 innerStartedAt,
            uint256 innerUpdatedAt,
            uint80 innerAnswered
        ) = IPriceFeed(phaseAggregator).getRoundData(
                uint80(uint64(_roundId))
            );
        return (
            _packRound(phase, innerRound),
            innerAnswer,
            innerStartedAt,
            innerUpdatedAt,
            _packRound(phase, innerAnswered)
        );
    }

    function latestRoundData()
        external
        view
        returns (
            uint80 roundId,
            int256 answer,
            uint256 startedAt,
            uint256 updatedAt,
            uint80 answeredInRound
        )
    {
        uint16 phase = uint16(currentPhaseId);
        (
            uint80 innerRound,
            int256 innerAnswer,
            uint256 innerStartedAt,
            uint256 innerUpdatedAt,
            uint80 innerAnswered
        ) = IPriceFeed(currentAggregator).latestRoundData();
        return (
            _packRound(phase, innerRound),
            innerAnswer,
            innerStartedAt,
            innerUpdatedAt,
            _packRound(phase, innerAnswered)
        );
    }

    function _packRound(uint16 phase, uint80 round) internal pure returns (uint80) {
        return uint80((uint256(phase) << PHASE_OFFSET) | uint64(round));
    }
}
