// SPDX-License-Identifier: LGPL-3.0-or-later
pragma solidity ^0.8.20;

interface IPgpProofVerifier {
    function verify(bytes32 statement, bytes calldata proof) external view returns (bool);
}

/// Applications bind a public statement to their network, contract, protocol and
/// action. Each version selects a separately deployed, gas-metered verifier.
contract PgpProofRegistry {
    struct Version {
        address verifier;
        bytes32 codeHash;
        uint64 gasLimit;
        bool enabled;
    }
    mapping(bytes32 => mapping(uint32 => Version)) public versions;
    address public immutable governor;
    event Registered(bytes32 indexed scheme, uint32 indexed version, address verifier, bytes32 codeHash, uint64 gasLimit);
    event Disabled(bytes32 indexed scheme, uint32 indexed version);

    constructor(address council) {
        require(council != address(0), "missing governor");
        governor = council;
    }

    function register(bytes32 scheme, uint32 version, address verifier, uint64 gasLimit) external {
        require(msg.sender == governor, "governor only");
        require(verifier.code.length != 0 && gasLimit != 0, "invalid verifier");
        require(versions[scheme][version].verifier == address(0), "version already used");
        bytes32 codeHash = verifier.codehash;
        versions[scheme][version] = Version(verifier, codeHash, gasLimit, true);
        emit Registered(scheme, version, verifier, codeHash, gasLimit);
    }

    function disable(bytes32 scheme, uint32 version) external {
        require(msg.sender == governor && versions[scheme][version].enabled, "cannot disable");
        versions[scheme][version].enabled = false;
        emit Disabled(scheme, version);
    }

    function verify(bytes32 scheme, uint32 version, bytes32 statement, bytes calldata proof) external view returns (bool) {
        Version memory v = versions[scheme][version];
        if (!v.enabled || v.verifier.codehash != v.codeHash) return false;
        bytes memory input = abi.encodeCall(IPgpProofVerifier.verify, (statement, proof));
        address verifier = v.verifier;
        uint256 gasLimit = v.gasLimit;
        bool valid;
        // Limit output copying to one ABI word even when a verifier reverts.
        assembly ("memory-safe") {
            let ok := staticcall(gasLimit, verifier, add(input, 32), mload(input), 0, 32)
            valid := and(and(ok, eq(returndatasize(), 32)), eq(mload(0), 1))
        }
        return valid;
    }
}
