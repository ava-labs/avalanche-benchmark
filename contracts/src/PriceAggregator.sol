// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

import {AggregatorV3Interface} from "./interfaces/AggregatorV3Interface.sol";

/// @title PriceAggregator
/// @notice Deployed on the main L1, one instance per pair, behind a
///         PriceFeedProxy. The Chainlink-shaped writer for the direct-publish
///         feed: a single authorized publisher pushes rounds, consumers read
///         AggregatorV3Interface. The aggregation is in the name and the
///         interface only; there is deliberately no multi-oracle consensus
///         here, that is the publisher's off-chain concern.
/// @dev No constructor. Config lives in fixed storage slots so the runtime
///      bytecode can be baked into genesis alloc and its storage seeded there:
///        slot 0 = authorized publisher address
///        slot 1 = description, short-string encoded: content left-aligned,
///                 byte length in the final byte (max 31 bytes)
///      Adding a constructor or immutables would move config into bytecode and
///      break genesis-alloc deployment.
contract PriceAggregator is AggregatorV3Interface {
    struct Round {
        int256 answer;
        uint64 updatedAt;
    }

    // slot 0: authorized publisher. Seeded via genesis alloc storage.
    address public publisher;

    // slot 1: short-string encoded description. Seeded via genesis alloc.
    bytes32 private descriptionData;

    // slot 2: latest round id, monotonic from 1.
    uint80 private latestRoundId;

    // slot 3: every round, keyed by round id.
    mapping(uint80 => Round) private rounds;

    /// @notice Chainlink's canonical answer event, so feed indexers and
    ///         monitors built for Chainlink aggregators work unchanged.
    event AnswerUpdated(
        int256 indexed current,
        uint256 indexed roundId,
        uint256 updatedAt
    );

    function submit(int256 answer) external {
        require(msg.sender == publisher, "not publisher");

        uint80 roundId = ++latestRoundId;
        rounds[roundId] = Round(answer, uint64(block.timestamp));

        emit AnswerUpdated(answer, roundId, block.timestamp);
    }

    function decimals() external pure returns (uint8) {
        return 8;
    }

    function description() external view returns (string memory) {
        bytes32 data = descriptionData;
        uint256 length = uint256(uint8(data[31]));
        bytes memory out = new bytes(length);
        for (uint256 i = 0; i < length; i++) {
            out[i] = data[i];
        }
        return string(out);
    }

    function version() external pure returns (uint256) {
        return 1;
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
        return _roundData(_roundId);
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
        return _roundData(latestRoundId);
    }

    // A push feed has no open/answer phase split, so startedAt == updatedAt
    // and answeredInRound == roundId, the same degenerate shape Chainlink's
    // own OCR aggregators report.
    function _roundData(
        uint80 _roundId
    ) internal view returns (uint80, int256, uint256, uint256, uint80) {
        Round storage round = rounds[_roundId];
        require(round.updatedAt > 0, "No data present");
        return (_roundId, round.answer, round.updatedAt, round.updatedAt, _roundId);
    }
}
