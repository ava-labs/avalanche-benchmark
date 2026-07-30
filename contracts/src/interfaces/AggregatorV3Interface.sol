// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

/// @notice Chainlink's AggregatorV3Interface, from smartcontractkit/chainlink-evm
///         (contracts/src/v0.8/shared/interfaces/AggregatorV3Interface.sol,
///         MIT licensed there as here). Signature-identical so consumers
///         written against Chainlink price feeds work against this kit's
///         feeds unchanged; only formatting differs.
interface AggregatorV3Interface {
    function decimals() external view returns (uint8);

    function description() external view returns (string memory);

    function version() external view returns (uint256);

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
        );

    function latestRoundData()
        external
        view
        returns (
            uint80 roundId,
            int256 answer,
            uint256 startedAt,
            uint256 updatedAt,
            uint80 answeredInRound
        );
}
