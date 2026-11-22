"""Sign owner-approved purchase intents via Clef. Run under the signer's OS account."""
import argparse
import json
import os
from pathlib import Path
import socket
import socketserver

from agent_tools import prepare_purchase, split_signature


def clef_sign(ipc_path, agent, typed_data):
    request = {"jsonrpc": "2.0", "id": 1, "method": "account_signTypedData",
               "params": [agent, typed_data]}
    with socket.socket(socket.AF_UNIX) as client:
        client.settimeout(120)
        client.connect(ipc_path)
        client.sendall(json.dumps(request).encode() + b"\n")
        with client.makefile("rb") as stream:
            response = json.loads(stream.readline(65536))
    if response.get("id") != 1 or "result" not in response:
        raise ValueError("clef_refused")
    sig = response["result"]
    split_signature(sig)
    return sig


def approved_generation(intent, policy):
    fields = {"taskId", "amountWei", "recipient"}
    if not fields <= intent.keys() <= fields | {"data"}:
        return None
    task = intent["taskId"].lower()
    recipient = intent["recipient"].lower()
    operation = intent.get("data", "0x").lower()
    tasks = policy.get("tasks", {})
    if task in tasks:
        # Explicit task entries take precedence even if the session rule is broader.
        exact = tasks[task]
        if (recipient == exact["recipient"].lower() and intent["amountWei"] == exact["amountWei"]
                and operation == exact.get("data", "0x").lower()):
            return int(exact["generation"])
        return None
    session = policy.get("nativePurchaseSession")
    if (session and recipient == session["recipient"].lower() and operation == "0x"
            and 0 < int(intent["amountWei"]) <= int(session["maxPerPurchaseWei"])):
        return int(session["generation"])
    return None


def sign_purchase(intent, policy, sign):
    generation = approved_generation(intent, policy)
    if generation is None:
        return {"status": "task_not_approved"}
    result = prepare_purchase(intent)
    if result["status"] != "within_grant":
        return result
    data = result["typedData"]
    if (int(data["domain"]["chainId"]) != int(policy["chainId"])
            or data["domain"]["verifyingContract"].lower() != policy["account"].lower()
            or data["message"]["agent"].lower() != policy["agent"].lower()
            or int(data["message"]["generation"]) != generation):
        return {"status": "signer_policy_mismatch"}
    return {"status": "signed", "taskId": intent["taskId"].lower(),
            "signature": sign(policy["clefIpc"], policy["agent"], data)}


def serve(socket_path, policy_path):
    class Handler(socketserver.StreamRequestHandler):
        def handle(self):
            self.request.settimeout(10)
            try:
                intent = json.loads(self.rfile.readline(1024 * 1024))
                policy = json.loads(Path(policy_path).read_text())
                result = sign_purchase(intent, policy, clef_sign)
            except (OSError, ValueError, KeyError, TypeError, AttributeError):
                result = {"status": "signer_refused"}
            self.wfile.write(json.dumps(result).encode() + b"\n")

    os.umask(0o077)
    with socketserver.UnixStreamServer(socket_path, Handler) as server:
        try:
            server.serve_forever()
        finally:
            os.unlink(socket_path)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socket", required=True)
    parser.add_argument("--policy", required=True)
    options = parser.parse_args()
    serve(options.socket, options.policy)
