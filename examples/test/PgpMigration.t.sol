// SPDX-License-Identifier: LGPL-3.0-or-later
pragma solidity ^0.8.20;

import "../agent-account/PgpAgentAccount.sol";
import "../proof-verification/PgpProofRegistry.sol";

interface Vm {
    function addr(uint256 key) external returns (address);
    function sign(uint256 key, bytes32 digest) external returns (uint8, bytes32, bytes32);
    function deal(address account, uint256 amount) external;
}

// Test adapter for a separately keyed verifier. This fixture uses ECDSA.
contract TestSignatureAdapter is IPgpProofVerifier {
    address immutable signer;
    constructor(address key) { signer = key; }

    function verify(bytes32 statement, bytes calldata signature) external view returns (bool) {
        (uint8 v, bytes32 r, bytes32 s) = abi.decode(signature, (uint8, bytes32, bytes32));
        return ecrecover(statement, v, r, s) == signer;
    }
}

contract PgpMigrationTest {
    Vm constant vm = Vm(address(uint160(uint256(keccak256("hevm cheat code")))));
    PgpAgentAccount account;
    PgpProofRegistry registry;
    address agent;
    bytes32 constant scheme = keccak256("test-scheme");
    bytes32 constant task = keccak256("purchase");
    bytes32 key;

    function setUp() public {
        account = new PgpAgentAccount(address(this));
        registry = new PgpProofRegistry(address(this));
        agent = vm.addr(1);
        key = keccak256(abi.encode(vm.addr(2)));
        registry.register(scheme, 1, address(new TestSignatureAdapter(vm.addr(2))), 50000);
        account.authorize(agent, address(42), keccak256(""), 10, uint64(block.timestamp + 60));
        vm.deal(address(account), 10);
    }

    function migrate() private {
        account.setSignaturePolicy(agent, PgpAgentAccount.SignaturePolicy(
            IPgpSignatureRegistry(address(registry)), scheme, 1, key
        ));
    }

    function testAlternatePaymentPreservesBudget() public {
        migrate();
        bytes32 statement = keccak256(abi.encode(key, account.digest(agent, task, 7)));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(2, statement);
        account.payWithSignature(agent, task, 7, "", abi.encode(v, r, s));
        (,,uint256 remaining,,) = account.grants(agent);
        require(remaining == 3 && account.settled(task), "settlement mismatch");
    }

    function testRoundTripInvalidatesOutstandingSignature() public {
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(1, account.digest(agent, task, 7));
        bytes memory callData = abi.encodeCall(account.pay, (agent, task, 7, "", v, r, s));
        migrate();
        (bool ok,) = address(account).call(callData);
        require(!ok, "legacy route remained active");
        account.setSignaturePolicy(agent, PgpAgentAccount.SignaturePolicy(
            IPgpSignatureRegistry(address(0)), bytes32(0), 0, bytes32(0)
        ));
        (ok,) = address(account).call(callData);
        require(!ok && !account.settled(task), "stale signature reused");
    }
}
