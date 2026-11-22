// SPDX-License-Identifier: LGPL-3.0-or-later
pragma solidity ^0.8.20;

interface IPgpSignatureRegistry {
    function verify(bytes32 scheme, uint32 version, bytes32 statement, bytes calldata signature)
        external view returns (bool);
}

/// Native-currency budget and exact call-data authorization for a session key.
contract PgpAgentAccount {
    address public immutable owner;
    struct Grant {
        address target;
        bytes32 dataHash;
        uint256 remaining;
        uint64 expiry;
        uint256 generation;
    }
    mapping(address => Grant) public grants;
    mapping(bytes32 => bool) public settled;
    mapping(address => uint64) public pausedExpiry;
    event PauseChanged(address indexed agent, bool paused, uint256 generation);

    function setPaused(address agent, bool pause) external {
        require(msg.sender == owner, "owner only");
        Grant storage grant = grants[agent];
        if (pause) {
            require(grant.expiry > block.timestamp, "no active grant");
            pausedExpiry[agent] = grant.expiry;
            grant.expiry = 0;
        } else {
            uint64 expiry = pausedExpiry[agent];
            require(expiry > block.timestamp, "no resumable grant");
            delete pausedExpiry[agent];
            grant.expiry = expiry;
        }
        emit PauseChanged(agent, pause, ++grant.generation);
    }
    struct SignaturePolicy {
        IPgpSignatureRegistry registry;
        bytes32 scheme;
        uint32 version;
        bytes32 keyCommitment;
    }
    mapping(address => SignaturePolicy) public signaturePolicies;
    event SignaturePolicyChanged(address indexed agent, uint256 generation, address registry,
        bytes32 scheme, uint32 version, bytes32 keyCommitment);

    /// Changes the verification policy without resetting the remaining spending allowance.
    /// A zero registry explicitly selects the original secp256k1 session key.
    function setSignaturePolicy(address agent, SignaturePolicy calldata policy) external {
        require(msg.sender == owner && agent != address(0), "owner and agent required");
        if (address(policy.registry) == address(0)) {
            require(policy.scheme == 0 && policy.version == 0 && policy.keyCommitment == 0, "invalid default");
        } else {
            require(address(policy.registry).code.length > 0 && policy.keyCommitment != 0, "invalid verifier");
        }
        signaturePolicies[agent] = policy;
        emit SignaturePolicyChanged(agent, ++grants[agent].generation, address(policy.registry),
            policy.scheme, policy.version, policy.keyCommitment);
    }

    bytes32 private constant DOMAIN = keccak256("EIP712Domain(uint256 chainId,address verifyingContract)");
    bytes32 private constant PAYMENT = keccak256("Payment(address agent,uint256 generation,bytes32 taskId,uint256 value)");
    event Authorized(address indexed agent, uint256 generation, address indexed target, bytes32 dataHash, uint256 budget, uint64 expiry);
    event Revoked(address indexed agent, uint256 generation);
    event Paid(bytes32 indexed taskId, address indexed agent, address indexed target, uint256 value);

    constructor(address controller) {
        require(controller != address(0), "missing owner");
        owner = controller;
    }
    receive() external payable {}

    function authorize(address agent, address target, bytes32 dataHash, uint256 budget, uint64 expiry) external {
        require(msg.sender == owner, "owner only");
        require(agent != address(0) && target != address(0) && target != address(this), "invalid address");
        require(expiry > block.timestamp, "expired");
        uint256 generation = grants[agent].generation + 1;
        delete pausedExpiry[agent];
        grants[agent] = Grant(target, dataHash, budget, expiry, generation);
        emit Authorized(agent, generation, target, dataHash, budget, expiry);
    }

    function revoke(address agent) external {
        require(msg.sender == owner, "owner only");
        Grant storage grant = grants[agent];
        grant.expiry = 0;
        delete pausedExpiry[agent];
        emit Revoked(agent, ++grant.generation);
    }

    function digest(address agent, bytes32 taskId, uint256 value) public view returns (bytes32) {
        bytes32 domain = keccak256(abi.encode(DOMAIN, block.chainid, address(this)));
        bytes32 payment = keccak256(abi.encode(PAYMENT, agent, grants[agent].generation, taskId, value));
        return keccak256(abi.encodePacked(hex"1901", domain, payment));
    }

    /// Any relayer may pay gas. A failed call rolls back budget and task status.
    function pay(address agent, bytes32 taskId, uint256 value, bytes calldata data, uint8 v, bytes32 r, bytes32 s) public {
        require(address(signaturePolicies[agent].registry) == address(0), "alternate signature required");
        require(agent != address(0) && ecrecover(digest(agent, taskId, value), v, r, s) == agent, "bad signature");
        _settle(agent, taskId, value, data);
    }

    function payWithSignature(address agent, bytes32 taskId, uint256 value, bytes calldata data,
        bytes calldata signature) external {
        SignaturePolicy memory policy = signaturePolicies[agent];
        require(address(policy.registry) != address(0), "no alternate verifier");
        bytes32 statement = keccak256(abi.encode(policy.keyCommitment, digest(agent, taskId, value)));
        require(policy.registry.verify(policy.scheme, policy.version, statement, signature), "bad signature");
        _settle(agent, taskId, value, data);
    }

    function _settle(address agent, bytes32 taskId, uint256 value, bytes calldata data) private {
        Grant storage grant = grants[agent];
        require(agent != address(0) && grant.expiry != 0 && block.timestamp < grant.expiry, "inactive grant");
        require(!settled[taskId], "task already paid");
        require(keccak256(data) == grant.dataHash && value <= grant.remaining, "outside grant");
        address target = grant.target;
        settled[taskId] = true;
        grant.remaining -= value;
        (bool ok,) = target.call{value: value}(data);
        require(ok, "payment failed");
        emit Paid(taskId, agent, target, value);
    }
}
