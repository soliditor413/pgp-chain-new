"""Local merchant HTTP service for purchasing a versioned text dataset on PGP."""
import argparse
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import sqlite3
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "agent-account"))
from agent_tools import NodeError, purchase_receipt, rpc


class Merchant:
    def __init__(self, db, dataset, price, recipient, chain):
        self.db = db
        self.dataset = dataset
        self.price = price
        self.recipient = recipient.lower()
        self.chain = chain
        self.account = os.environ["PGP_ACCOUNT"].lower()
        self.agent = os.environ["PGP_AGENT"].lower()
        for address in (self.account, self.agent, self.recipient):
            if not re.fullmatch("0x[0-9a-f]{40}", address) or int(address, 16) == 0:
                raise ValueError("invalid account, agent or merchant address")
        if not 0 < int(price) < 2**256 or str(int(price)) != price or chain <= 0:
            raise ValueError("invalid price or chain")
        db.execute("""CREATE TABLE IF NOT EXISTS orders (
            request_id TEXT PRIMARY KEY, task TEXT UNIQUE NOT NULL, quote TEXT NOT NULL,
            dataset TEXT NOT NULL, settled_tx TEXT)""")

    def quote(self, request_id):
        if not isinstance(request_id, str) or not re.fullmatch("[a-zA-Z0-9_-]{1,80}", request_id):
            return 400, {"status": "invalid_request_id"}
        with self.db:
            self.db.execute("BEGIN IMMEDIATE")
            row = self.db.execute("SELECT quote FROM orders WHERE request_id=?", (request_id,)).fetchone()
            if row:
                return 200, json.loads(row[0])
            data = self.dataset.read_text(encoding="utf-8")
            task = "0x" + secrets.token_hex(32)
            quote = dict(taskId=task, recipient=self.recipient, amountWei=self.price, data="0x",
                         chainId=str(self.chain), account=self.account, agent=self.agent,
                         sha256=hashlib.sha256(data.encode()).hexdigest())
            self.db.execute("INSERT INTO orders VALUES (?,?,?,?,NULL)",
                            (request_id, task, json.dumps(quote), data))
        return 201, quote

    def deliver(self, task, tx):
        if not all(isinstance(x, str) and re.fullmatch("0x[0-9a-fA-F]{64}", x) for x in (task, tx)):
            return 400, {"status": "invalid_identifier"}
        task, tx = task.lower(), tx.lower()
        row = self.db.execute("SELECT quote,dataset,settled_tx FROM orders WHERE task=?", (task,)).fetchone()
        if not row:
            return 404, {"status": "unknown_task"}
        quote, dataset, settled = json.loads(row[0]), row[1], row[2]
        if (quote["account"], quote["agent"], quote["chainId"]) != (self.account, self.agent, str(self.chain)):
            return 409, {"status": "order_context_changed"}
        if settled and settled != tx:
            return 409, {"status": "different_settlement"}
        if not settled:
            if int(rpc("eth_chainId", []), 16) != self.chain:
                return 503, {"status": "wrong_node_chain"}
            receipt = purchase_receipt(dict(taskId=task, txHash=tx, includeFinality=True))
            state = receipt["status"]
            if state in ("awaiting_receipt", "chain_changed"):
                return 202, {"status": state}
            if (state != "payment_included" or receipt["recipient"].lower() != quote["recipient"]
                    or receipt["amountWei"] != quote["amountWei"]):
                return 409, {"status": "payment_mismatch"}
            if receipt["finality"] != "finalized":
                return 202, {"status": "awaiting_finality"}
            with self.db:
                cursor = self.db.execute("UPDATE orders SET settled_tx=? WHERE task=? AND (settled_tx IS NULL OR settled_tx=?)",
                                         (tx, task, tx))
                if not cursor.rowcount:
                    return 409, {"status": "different_settlement"}
        return 200, dict(status="delivered", taskId=task, txHash=tx, sha256=quote["sha256"], text=dataset)


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        auth = self.headers.get("Authorization", "").encode()
        if not hmac.compare_digest(auth, self.server.authorization):
            self.reply(401, {"status": "unauthorized"})
            return
        try:
            size = int(self.headers.get("Content-Length", "0"))
            if not 0 < size <= 4096:
                self.reply(400, {"status": "invalid_size"})
                return
            body = json.loads(self.rfile.read(size))
            match = re.fullmatch(r"/v1/orders/(0x[0-9a-fA-F]{64})/delivery", self.path)
            if not isinstance(body, dict):
                result = 400, {"status": "invalid_body"}
            elif self.path == "/v1/orders" and set(body) == {"requestId"}:
                result = self.server.merchant.quote(body["requestId"])
            elif match and set(body) == {"txHash"}:
                result = self.server.merchant.deliver(match[1], body["txHash"])
            elif self.path == "/v1/orders" or match:
                result = 400, {"status": "invalid_body"}
            else:
                result = 404, {"status": "unknown_route"}
        except (ValueError, UnicodeError):
            result = 400, {"status": "invalid_data"}
        except (OSError, sqlite3.Error, NodeError):
            result = 503, {"status": "service_unavailable"}
        self.reply(*result)

    def reply(self, status, body):
        data = json.dumps(body, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--database", type=Path, required=True)
    parser.add_argument("--dataset", type=Path, required=True)
    parser.add_argument("--price-wei", required=True)
    parser.add_argument("--recipient", required=True)
    parser.add_argument("--chain-id", type=int, required=True)
    parser.add_argument("--port", type=int, default=8787)
    args = parser.parse_args()
    token = os.environ["PGP_SERVICE_TOKEN"]
    if len(token) < 32 or not token.isascii():
        parser.error("PGP_SERVICE_TOKEN must contain at least 32 ASCII characters")
    os.umask(0o077)
    with sqlite3.connect(args.database) as db, HTTPServer(("127.0.0.1", args.port), Handler) as server:
        server.authorization = ("Bearer " + token).encode()
        server.merchant = Merchant(db, args.dataset, args.price_wei, args.recipient, args.chain_id)
        server.serve_forever()
