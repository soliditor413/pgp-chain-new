// SPDX-License-Identifier: LGPL-3.0-or-later
pragma solidity ^0.8.20;

interface IPgpCouncilWallet {
    function isValidSignature(bytes32 hash, bytes calldata signature) external view returns (bytes4);
}

/// Deploy as the owner of an agent account. Independent controller keys
/// approve a specific target, calldata and salt before its delay starts.
contract PgpAccountCouncil {
    mapping(address => bool) public member;
    uint256 public immutable quorum;
    uint256 public immutable delay;
    uint256 public epoch;
    struct Proposal {
        uint256 approvals;
        uint256 readyAt;
        bool closed;
    }
    mapping(bytes32 => Proposal) public proposals;
    mapping(bytes32 => mapping(address => bool)) public approved;
    mapping(address => uint256) public voteNonce;
    bytes32 private constant DOMAIN = keccak256("EIP712Domain(uint256 chainId,address verifyingContract)");
    bytes32 private constant BALLOT = keccak256(
        "Ballot(address member,bytes32 proposalId,bool support,uint256 nonce,uint256 deadline)"
    );

    event Vote(bytes32 indexed id, address indexed signer, bool support);
    event Scheduled(bytes32 indexed id, address indexed target, bytes data, uint256 readyAt);
    event Executed(bytes32 indexed id);
    event Rotated(address indexed previous, address indexed replacement, uint256 epoch);

    constructor(address[] memory signers, uint256 required, uint256 delaySeconds) {
        require(required >= 2 && required <= signers.length && delaySeconds > 0, "invalid policy");
        quorum = required;
        delay = delaySeconds;
        for (uint256 i; i < signers.length; ++i) {
            address signer = signers[i];
            require(signer != address(0) && !member[signer], "invalid signer");
            member[signer] = true;
        }
    }

    function proposalId(address target, bytes calldata data, bytes32 salt) public view returns (bytes32) {
        return keccak256(abi.encode(block.chainid, address(this), epoch, target, data, salt));
    }

    function vote(address target, bytes calldata data, bytes32 salt, bool support) external {
        _vote(msg.sender, target, data, salt, support);
    }

    function ballotDigest(address signer, bytes32 id, bool support, uint256 deadline) public view returns (bytes32) {
        bytes32 domain = keccak256(abi.encode(DOMAIN, block.chainid, address(this)));
        bytes32 ballot = keccak256(abi.encode(BALLOT, signer, id, support, voteNonce[signer], deadline));
        return keccak256(abi.encodePacked(hex"1901", domain, ballot));
    }

    /// An ERC-1271 member wallet selects and migrates its own signature algorithm.
    function relayVote(address signer, address target, bytes calldata data, bytes32 salt, bool support,
        uint256 deadline, bytes calldata signature) external {
        require(member[signer] && signer.code.length != 0 && block.timestamp <= deadline, "invalid ballot");
        bytes32 digest = ballotDigest(signer, proposalId(target, data, salt), support, deadline);
        require(IPgpCouncilWallet(signer).isValidSignature(digest, signature)
            == IPgpCouncilWallet.isValidSignature.selector, "invalid signature");
        _vote(signer, target, data, salt, support);
    }

    function _vote(address signer, address target, bytes calldata data, bytes32 salt, bool support) private {
        require(member[signer], "member only");
        bytes32 id = proposalId(target, data, salt);
        Proposal storage p = proposals[id];
        require(!p.closed && approved[id][signer] != support, "invalid vote");
        approved[id][signer] = support;
        ++voteNonce[signer];
        if (support) ++p.approvals;
        else --p.approvals;
        if (p.approvals < quorum) p.readyAt = 0;
        else if (p.readyAt == 0) {
            p.readyAt = block.timestamp + delay;
            emit Scheduled(id, target, data, p.readyAt);
        }
        emit Vote(id, signer, support);
    }

    function execute(address target, bytes calldata data, bytes32 salt) external {
        bytes32 id = proposalId(target, data, salt);
        Proposal storage p = proposals[id];
        require(!p.closed && p.readyAt != 0 && block.timestamp >= p.readyAt, "not ready");
        require(target.code.length != 0, "contract required");
        p.closed = true;
        (bool ok, bytes memory reason) = target.call(data);
        if (!ok) assembly { revert(add(reason, 32), mload(reason)) }
        emit Executed(id);
    }

    /// The remaining quorum can replace a lost key. Pending votes become stale.
    function rotate(address previous, address replacement) external {
        require(msg.sender == address(this), "council only");
        require(member[previous] && !member[replacement] && replacement != address(0), "invalid rotation");
        member[previous] = false;
        member[replacement] = true;
        emit Rotated(previous, replacement, ++epoch);
    }
}
