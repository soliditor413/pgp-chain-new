import os
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

import service


class DeliveryTest(unittest.TestCase):
    def test_pinned_quote_and_finalized_idempotent_delivery(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, PGP_ACCOUNT="0x"+"11"*20, PGP_AGENT="0x"+"22"*20):
            db = sqlite3.connect(Path(tmp)/"orders.sqlite")
            self.addCleanup(db.close)
            data = Path(tmp)/"data"
            data.write_text("first")
            recipient = "0x"+"33"*20
            merchant = service.Merchant(db, data, "60", recipient, 1)
            code, quote = merchant.quote("r1")
            self.assertEqual(code, 201)
            data.write_text("second")
            self.assertEqual(merchant.quote("r1"), (200, quote))
            task, tx = quote["taskId"], "0x"+"44"*32
            receipt = dict(status="payment_included", amountWei="60", recipient=recipient, finality="unknown")
            with patch.object(service, "rpc", return_value="0x1"), patch.object(service, "purchase_receipt", return_value=receipt):
                self.assertEqual(merchant.deliver(task, tx)[0], 202)
                receipt.update(finality="finalized", amountWei="59")
                self.assertEqual(merchant.deliver(task, tx)[0], 409)
                receipt.update(amountWei="60")
                result = merchant.deliver(task, tx)
                self.assertEqual(result[1]["text"], "first")
            self.assertEqual(merchant.deliver(task, tx), result)
            self.assertEqual(merchant.deliver(task, "0x"+"55"*32)[0], 409)
