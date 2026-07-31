// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

/// @notice The kit's price feed read interface. Signature-for-signature
///         compatible with Chainlink's AggregatorV3Interface (selectors are
///         derived from function signatures only, so consumers written
///         against Chainlink feeds interoperate with these feeds unchanged,
///         and vice versa). The name differs because this file is original;
///         only the ABI shape is shared.
interface IPriceFeed {
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
