import json
import os
import unittest
from unittest.mock import patch

import agent_tools as agent


class PurchaseTests(unittest.TestCase):
    def setUp(self):
        self.account = "0x" + "11" * 20
        self.key = "0x" + "22" * 20
        self.target = "0x" + "33" * 20
        self.task = "0x" + "44" * 32
        self.args = dict(recipient=self.target, amountWei="20", taskId=self.task)
        self.at = "0x" + "55" * 32
        self.paid = False
        self.expiry = 200
        self.calls = []
        env = patch.dict(os.environ, PGP_ACCOUNT=self.account, PGP_AGENT=self.key)
        env.start()
        self.addCleanup(env.stop)
        mock = patch.object(agent, "rpc", side_effect=self.rpc)
        mock.start()
        self.addCleanup(mock.stop)

    def rpc(self, method, params):
        self.calls.append(method)
        if method == "eth_getBlockByNumber":
            return dict(hash=self.at, timestamp=hex(100))
        if method == "eth_chainId":
            return hex(860621)
        self.assertEqual(method, "eth_call")
        self.assertEqual(params[1], dict(blockHash=self.at, requireCanonical=True))
        if params[0]["data"].startswith("0xd945af1d"):
            return "0x" + format(self.paid, "064x")
        fields = [self.target[2:], agent.EMPTY_CALL_HASH, "14", hex(self.expiry)[2:], "2"]
        return "0x" + "".join(v.zfill(64) for v in fields)

    def prepare(self, **updates):
        response = agent.dispatch({"method": "tools/call", "params": {
            "name": "prepare_purchase", "arguments": {**self.args, **updates}}})
        self.assertFalse(response["isError"])
        return json.loads(response["content"][0]["text"])

    def test_aggregate_budget_checked_at_one_block(self):
        small = {**self.args, "amountWei": "10"}
        second = {**small, "taskId": "0x" + "99" * 32}
        result = agent.check_batch({"purchases": [small, second]})
        self.assertEqual(len(result["typedData"]), 2)
        self.assertEqual(self.calls.count("eth_getBlockByNumber"), 1)
        self.assertEqual(agent.check_batch({"purchases": [self.args, second]}),
                         {"status": "needs_authorization", "reasons": ["budget"]})
        self.assertEqual(agent.check_batch({"purchases": [small, small]}), {"status": "duplicate_task"})

    def test_signed_intent(self):
        typed = self.prepare()["typedData"]
        self.assertEqual(typed["domain"], dict(chainId="860621", verifyingContract=self.account))
        self.assertEqual(typed["message"], dict(agent=self.key, generation="2", value="20", taskId=self.task))

    def test_no_signature_when_policy_fails(self):
        for updates, reason in [({"amountWei": "21"}, "budget"), ({"recipient": self.account}, "target_or_operation")]:
            result = self.prepare(**updates)
            self.assertIn(reason, result["reasons"])
            self.assertNotIn("typedData", result)
        self.expiry = 100
        self.assertIn("grant_expired_or_missing", self.prepare()["reasons"])
        self.paid = True
        self.assertEqual(self.prepare(), {"status": "already_paid"})
        self.assertNotIn("eth_chainId", self.calls)


if __name__ == "__main__":
    unittest.main()
