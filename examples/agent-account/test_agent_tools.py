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

    def test_lost_submission_response_preserves_task_and_requests_reconciliation(self):
        env = dict(PGP_SPONSOR=self.key, PGP_SPONSOR_MAX_GAS="21000", PGP_SPONSOR_MAX_FEE_WEI="21000")
        with patch.dict(os.environ, env), patch.object(agent, "rpc", side_effect=["0x5208", "0x1", TimeoutError()]):
            result = agent.sponsor_transaction({"to": self.account, "data": "0x"}, {"taskId": self.task})
        self.assertEqual(result["status"], "submission_unconfirmed")
        self.assertEqual(result["taskId"], self.task)
        self.assertNotIn("txHash", result)

    def test_preview_separates_forwarded_value_from_sponsor_fee(self):
        items = [self.args, {**self.args, "amountWei": "0", "data": "0x12345678"}]
        with patch.object(agent, "rpc", side_effect=["0x", "0x5208", "0x2"]) as node:
            preview = agent.preview_transaction({"to": self.account}, self.at, items)
        self.assertEqual(preview["nativeValueForwardedWei"], "20")
        self.assertEqual(preview["operations"][1]["callData"], "0x12345678")
        self.assertEqual(preview["feeEstimate"]["feeWei"], "42000")
        self.assertEqual([call.args[0] for call in node.call_args_list],
                         ["eth_call", "eth_estimateGas", "eth_gasPrice"])

    def test_reverted_receipt_includes_transaction_fee(self):
        receipt = {"status": "0x0", "blockHash": self.at, "gasUsed": "0x5208",
                   "effectiveGasPrice": "0x3", "from": self.key}
        args = {"taskId": self.task, "txHash": "0x" + "66" * 32}
        with patch.object(agent, "rpc", return_value=receipt):
            result = agent.purchase_receipt(args)
        self.assertEqual(result["status"], "reverted")
        self.assertEqual(result["payer"], self.key)
        self.assertEqual(result["transactionExecutionFeeWei"], "63000")
        del receipt["effectiveGasPrice"]
        with patch.object(agent, "rpc", return_value=receipt):
            self.assertNotIn("transactionExecutionFeeWei", agent.purchase_receipt(args))

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
