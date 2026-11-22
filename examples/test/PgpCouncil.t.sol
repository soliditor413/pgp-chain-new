// SPDX-License-Identifier: LGPL-3.0-or-later
pragma solidity ^0.8.20;

import "../agent-account/PgpAccountCouncil.sol";

interface Clock {
    function warp(uint256 timestamp) external;
}

contract TestCouncilWallet is IPgpCouncilWallet {
    bytes32 public ballot;

    function allow(bytes32 hash) external { ballot = hash; }

    function isValidSignature(bytes32 hash, bytes calldata) external view returns (bytes4) {
        return hash == ballot ? this.isValidSignature.selector : bytes4(0);
    }
}

contract PgpCouncilTest {
    Clock constant clock = Clock(address(uint160(uint256(keccak256("hevm cheat code")))));

    function testWalletVoteHonorsQuorumDelayAndRotation() public {
        TestCouncilWallet wallet = new TestCouncilWallet();
        address[] memory members = new address[](3);
        members[0] = address(this);
        members[1] = address(wallet);
        members[2] = address(21);
        PgpAccountCouncil council = new PgpAccountCouncil(members, 2, 60);
        bytes memory data = abi.encodeCall(council.rotate, (members[2], address(22)));
        bytes32 salt = keccak256("recover");
        bytes32 id = council.proposalId(address(council), data, salt);
        uint256 deadline = block.timestamp + 100;
        bytes memory execute = abi.encodeCall(council.execute, (address(council), data, salt));

        council.vote(address(council), data, salt, true);
        (bool ok,) = address(council).call(execute);
        require(!ok, "quorum");
        wallet.allow(council.ballotDigest(address(wallet), id, true, deadline));
        council.relayVote(address(wallet), address(council), data, salt, true, deadline, "");
        (ok,) = address(council).call(execute);
        require(!ok, "delay");
        require(council.voteNonce(address(wallet)) == 1, "nonce");
        clock.warp(block.timestamp + 60);
        council.execute(address(council), data, salt);
        require(council.epoch() == 1 && council.member(address(22)) && !council.member(members[2]), "rotation");
        bytes memory stale = abi.encodeCall(council.relayVote,
            (address(wallet), address(council), data, salt, true, deadline, ""));
        (ok,) = address(council).call(stale);
        require(!ok, "stale ballot");
    }
}
