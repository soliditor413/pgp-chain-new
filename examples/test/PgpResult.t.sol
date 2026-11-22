// SPDX-License-Identifier: LGPL-3.0-or-later
pragma solidity ^0.8.20;

import "./PgpMigration.t.sol";
import "../proof-verification/PgpResultInbox.sol";

contract PgpResultTest {
    Vm constant vm = Vm(address(uint160(uint256(keccak256("hevm cheat code")))));

    function testBoundResultVerifiedOnce() public {
        bytes32 scheme = keccak256("test-attestation");
        bytes32 task = keccak256("quote-request");
        bytes32 output = keccak256("service-result");
        PgpProofRegistry registry = new PgpProofRegistry(address(this));
        registry.register(scheme, 1, address(new TestSignatureAdapter(vm.addr(3))), 50000);
        PgpResultInbox inbox = new PgpResultInbox(registry, scheme, 1, address(this));
        inbox.request(task, keccak256("salted-input"));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(3, inbox.statement(task, output));
        bytes memory proof = abi.encode(v, r, s);
        (bool ok,) = address(inbox).call(abi.encodeCall(inbox.submit, (task, bytes32(0), proof)));
        require(!ok, "accepted different result");
        inbox.submit(task, output, proof);
        (, bool verified,,bytes32 stored) = inbox.results(task);
        require(verified && stored == output, "missing result");
        (ok,) = address(inbox).call(abi.encodeCall(inbox.submit, (task, output, proof)));
        require(!ok, "duplicate accepted");
    }
}
