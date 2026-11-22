// SPDX-License-Identifier: LGPL-3.0-or-later
pragma solidity ^0.8.20;

import "./PgpMigration.t.sol";
import "../agent-account/PgpPurchaseBatch.sol";

contract PgpSettlementTest {
    Vm constant vm = Vm(address(uint160(uint256(keccak256("hevm cheat code")))));
    PgpAgentAccount account;
    PgpPurchaseBatch batch;
    address agent;
    address recipient = address(128);
    uint64 expiry;

    function setUp() public {
        account = new PgpAgentAccount(address(this));
        batch = new PgpPurchaseBatch();
        agent = vm.addr(1);
        expiry = uint64(block.timestamp + 60);
        account.authorize(agent, recipient, keccak256(""), 10, expiry);
        vm.deal(address(account), 10);
    }

    function payment(bytes32 id, uint256 amount) private returns (PgpPurchaseBatch.Payment memory) {
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(1, account.digest(agent, id, amount));
        return PgpPurchaseBatch.Payment(id, amount, "", v, r, s);
    }

    function testAtomicSettlementAndReplay() public {
        bytes32 first = keccak256("one");
        bytes32 second = keccak256("two");
        PgpPurchaseBatch.Payment[] memory items = new PgpPurchaseBatch.Payment[](2);
        items[0] = payment(first, 6);
        items[1] = payment(second, 5);
        (bool ok,) = address(batch).call(abi.encodeCall(batch.settle, (account, agent, items)));
        require(!ok && !account.settled(first) && recipient.balance == 0, "partial settlement");
        items[1] = payment(second, 4);
        batch.settle(account, agent, items);
        require(account.settled(first) && account.settled(second) && recipient.balance == 10, "missing payment");
        (ok,) = address(batch).call(abi.encodeCall(batch.settle, (account, agent, items)));
        require(!ok, "replayed");
    }

    function testPauseAndRevocation() public {
        PgpPurchaseBatch.Payment memory p = payment(keccak256("purchase"), 2);
        bytes memory request = abi.encodeCall(account.pay, (agent, p.taskId, p.value, p.data, p.v, p.r, p.s));
        account.setPaused(agent, true);
        (bool ok,) = address(account).call(request);
        require(!ok, "paused payment");
        account.setPaused(agent, false);
        (ok,) = address(account).call(request);
        require(!ok, "stale signature");
        (,,uint256 remaining,uint64 restored,) = account.grants(agent);
        require(remaining == 10 && restored == expiry, "grant changed");
        account.setPaused(agent, true);
        account.revoke(agent);
        (ok,) = address(account).call(abi.encodeCall(account.setPaused, (agent, false)));
        require(!ok, "revoked grant resumed");
    }
}
