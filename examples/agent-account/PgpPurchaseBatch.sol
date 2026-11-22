// SPDX-License-Identifier: LGPL-3.0-or-later
pragma solidity ^0.8.20;

import "./PgpAgentAccount.sol";

/// A sponsor can settle several independently signed payments atomically.
contract PgpPurchaseBatch {
    struct Payment {
        bytes32 taskId;
        uint256 value;
        bytes data;
        uint8 v;
        bytes32 r;
        bytes32 s;
    }

    function settle(PgpAgentAccount account, address agent, Payment[] calldata payments) external {
        require(payments.length != 0, "empty batch");
        for (uint256 i; i < payments.length; ++i) {
            Payment calldata p = payments[i];
            account.pay(agent, p.taskId, p.value, p.data, p.v, p.r, p.s);
        }
    }
}
