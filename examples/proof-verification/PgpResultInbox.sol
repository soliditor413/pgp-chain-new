// SPDX-License-Identifier: LGPL-3.0-or-later
pragma solidity ^0.8.20;

import "./PgpProofRegistry.sol";

/// Example application for an off-chain service with a versioned verifier.
/// A verifier for the chosen scheme must validate the application's statement.
contract PgpResultInbox {
    PgpProofRegistry public immutable registry;
    bytes32 public immutable scheme;
    uint32 public immutable version;
    address public immutable requester;

    struct Result {
        bool requested;
        bool verified;
        bytes32 inputCommitment;
        bytes32 outputCommitment;
    }
    mapping(bytes32 => Result) public results;
    event Requested(bytes32 indexed taskId, bytes32 indexed inputCommitment);
    event Verified(bytes32 indexed taskId, bytes32 indexed outputCommitment);

    constructor(PgpProofRegistry verifiers, bytes32 schemeId, uint32 verifierVersion, address owner) {
        require(address(verifiers).code.length != 0 && owner != address(0), "invalid configuration");
        registry = verifiers;
        scheme = schemeId;
        version = verifierVersion;
        requester = owner;
    }

    function request(bytes32 taskId, bytes32 inputCommitment) external {
        require(msg.sender == requester && !results[taskId].requested, "invalid request");
        results[taskId] = Result(true, false, inputCommitment, 0);
        emit Requested(taskId, inputCommitment);
    }

    function statement(bytes32 taskId, bytes32 outputCommitment) public view returns (bytes32) {
        return keccak256(abi.encode(block.chainid, address(this), scheme, version,
            taskId, results[taskId].inputCommitment, outputCommitment));
    }

    function submit(bytes32 taskId, bytes32 outputCommitment, bytes calldata proof) external {
        Result storage result = results[taskId];
        require(result.requested && !result.verified, "task unavailable");
        require(registry.verify(scheme, version, statement(taskId, outputCommitment), proof), "invalid proof");
        result.outputCommitment = outputCommitment;
        result.verified = true;
        emit Verified(taskId, outputCommitment);
    }
}
