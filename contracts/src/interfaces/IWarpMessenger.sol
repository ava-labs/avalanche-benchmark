// SPDX-License-Identifier: MIT
pragma solidity ^0.8.25;

// Vendored from ava-labs/subnet-evm
// precompile/contracts/warp/warpbindings/IWarpMessenger.sol
// The Warp precompile lives at 0x0200000000000000000000000000000000000005.
// Signatures must match subnet-evm exactly or the precompile calls revert.

struct WarpMessage {
    bytes32 sourceChainID;
    address originSenderAddress;
    bytes payload;
}

struct WarpBlockHash {
    bytes32 sourceChainID;
    bytes32 blockHash;
}

interface IWarpMessenger {
    event SendWarpMessage(
        address indexed sender,
        bytes32 indexed messageID,
        bytes message
    );

    function sendWarpMessage(
        bytes calldata payload
    ) external returns (bytes32 messageID);

    function getVerifiedWarpMessage(
        uint32 index
    ) external view returns (WarpMessage calldata message, bool valid);

    function getVerifiedWarpBlockHash(
        uint32 index
    ) external view returns (WarpBlockHash calldata warpBlockHash, bool valid);

    function getBlockchainID() external view returns (bytes32 blockchainID);
}
