import unittest
from unittest.mock import Mock, patch
import session_signer


class SignerTests(unittest.TestCase):
    def setUp(self):
        self.task = "0x" + "aa" * 32
        self.intent = dict(recipient="0x" + "33" * 20, amountWei="80", taskId=self.task)
        self.policy = dict(chainId=860621, account="0x" + "11" * 20,
                           agent="0x" + "22" * 20, clefIpc="/session/clef.ipc", tasks={
                               self.task: dict(recipient=self.intent["recipient"], amountWei="80", generation=1)})
        self.typed = dict(domain=dict(chainId="860621", verifyingContract=self.policy["account"]),
                          message=dict(agent=self.policy["agent"], generation="1"))
        self.sign = Mock(return_value="signature")

    def test_changed_intent_refused_before_rpc(self):
        with patch.object(session_signer, "prepare_purchase") as prepare:
            for change in [dict(recipient=self.policy["account"]), dict(amountWei="81"), dict(data="0x12345678"),
                           dict(taskId="0x" + "bb"*32)]:
                result = session_signer.sign_purchase({**self.intent, **change}, self.policy, self.sign)
                self.assertEqual(result["status"], "task_not_approved")
            prepare.assert_not_called()
        self.sign.assert_not_called()

    def test_standing_grant_allows_new_tasks_only_within_pinned_scope(self):
        self.policy["nativePurchaseSession"] = dict(recipient=self.intent["recipient"],
                                                    maxPerPurchaseWei="100", generation=1)
        fresh = {**self.intent, "taskId": "0x" + "cc" * 32}
        with patch.object(session_signer, "prepare_purchase",
                          return_value=dict(status="within_grant", typedData=self.typed)) as prepare:
            for invalid in [{**fresh, "amountWei": "101"}, {**fresh, "amountWei": "0"},
                            {**fresh, "recipient": self.policy["agent"]},
                            {**fresh, "data": "0x01"}, {**self.intent, "amountWei": "81"}]:
                self.assertEqual(session_signer.sign_purchase(invalid, self.policy, self.sign)["status"],
                                 "task_not_approved")
            prepare.assert_not_called()
            for task in [fresh, {**fresh, "taskId": "0x" + "dd" * 32}]:
                self.assertEqual(session_signer.sign_purchase(task, self.policy, self.sign)["status"], "signed")
            self.typed["message"]["generation"] = "2"
            self.assertEqual(session_signer.sign_purchase(fresh, self.policy, self.sign)["status"],
                             "signer_policy_mismatch")
            prepare.return_value = {"status": "needs_authorization", "reasons": ["budget"]}
            self.assertEqual(session_signer.sign_purchase(fresh, self.policy, self.sign), prepare.return_value)
            self.assertEqual(self.sign.call_count, 2)

    def test_current_grant_and_domain_required(self):
        with patch.object(session_signer, "prepare_purchase",
                          return_value=dict(status="within_grant", typedData=self.typed)):
            for change in [dict(chainId=1), dict(agent=self.policy["account"]),
                           dict(account=self.policy["agent"])]:
                self.assertEqual(session_signer.sign_purchase(self.intent, {**self.policy, **change},
                                 self.sign)["status"], "signer_policy_mismatch")
            self.typed["message"]["generation"] = "2"
            self.assertEqual(session_signer.sign_purchase(self.intent, self.policy, self.sign)["status"],
                             "signer_policy_mismatch")
            self.sign.assert_not_called()
            self.typed["message"]["generation"] = "1"
            self.assertEqual(session_signer.sign_purchase(self.intent, self.policy, self.sign)["status"], "signed")
            self.sign.assert_called_once_with(self.policy["clefIpc"], self.policy["agent"], self.typed)


if __name__ == "__main__":
    unittest.main()
