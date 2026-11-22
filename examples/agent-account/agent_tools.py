"""MCP tools for authorized native-currency purchases on PGP."""
import json
import os
import re
import socket
import sys
from urllib.parse import urlsplit
from urllib.request import ProxyHandler, Request, build_opener, urlopen

PAID_EVENT = "0x37db9851b6c9a32f8ddcf4734e6526de7c85268ef735f7883ea70dc8a39c9c85"
EMPTY_CALL_HASH = "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"
TOOL = {
    "name": "check_purchase",
    "description": "Check a native PGP payment to a service before requesting a signature. Amount is in wei.",
    "inputSchema": {
        "type": "object", "additionalProperties": False,
        "properties": {key: {"type": "string", "pattern": pattern} for key, pattern in {
            "recipient": "^0x[0-9a-fA-F]{40}$", "amountWei": "^(0|[1-9][0-9]{0,77})$", "data": "^0x(?:[0-9a-fA-F]{2})*$",
            "taskId": "^0x[0-9a-fA-F]{64}$"}.items()},
        "required": ["recipient", "amountWei", "taskId"]}}


class NodeError(Exception):
    def __init__(self, method, code):
        self.method = method
        self.code = code


def rpc(method, params):
    body = json.dumps(dict(jsonrpc="2.0", id=1, method=method, params=params)).encode()
    request = Request(os.environ["PGP_RPC_URL"], body, {"Content-Type": "application/json"})
    # Keep local node calls on loopback even when macOS configures a system proxy.
    opener = urlopen
    if urlsplit(request.full_url).hostname in {"127.0.0.1", "::1", "localhost"}:
        opener = build_opener(ProxyHandler({})).open
    with opener(request, timeout=10) as response:
        payload = json.load(response)
    if isinstance(payload.get("error"), dict):
        raise NodeError(method, payload["error"].get("code"))
    return payload["result"]


def check_purchase(args, block=None):
    schema = TOOL["inputSchema"]
    if not set(schema["required"]) <= args.keys() <= schema["properties"].keys() or any(
        not isinstance(args[k], str) or not re.fullmatch(v["pattern"], args[k])
        for k, v in schema["properties"].items() if k in args
    ):
        raise ValueError("invalid purchase arguments")
    data = args.get("data", "0x")
    if int(args["amountWei"]) >= 2**256 or (args["amountWei"] == "0" and data == "0x"):
        raise ValueError("invalid native amount")
    account, agent = (os.environ[k] for k in ("PGP_ACCOUNT", "PGP_AGENT"))
    if not all(re.fullmatch(r"0x[0-9a-fA-F]{40}", addr) for addr in (agent, account)):
        raise ValueError("invalid configured account or agent")
    block = block or rpc("eth_getBlockByNumber", ["latest", False])
    at = {"blockHash": block["hash"], "requireCanonical": True}

    def read(data, words):
        result = rpc("eth_call", [{"to": account, "data": data}, at])
        if not re.fullmatch("0x[0-9a-fA-F]{" + str(64 * words) + "}", result):
            raise ValueError("unexpected account response")
        return [result[i:i + 64].lower() for i in range(2, len(result), 64)]

    paid, = read("0xd945af1d" + args["taskId"][2:], 1)
    if int(paid, 16):
        return {"status": "already_paid"}
    agent_word = agent[2:].zfill(64)
    target, call_hash, budget, expiry, generation = read("0xb869cea3" + agent_word, 5)
    reasons = []
    expected_hash = EMPTY_CALL_HASH if data == "0x" else rpc("web3_sha3", [data])[2:].lower()
    if target[-40:] != args["recipient"][2:].lower() or call_hash != expected_hash:
        reasons.append("target_or_operation")
    if int(expiry, 16) <= int(block["timestamp"], 16):
        reason = "grant_expired_or_missing"
        if int(expiry, 16) == 0 and int(target, 16):
            paused_until, = read("0x851c1f81" + agent_word, 1)
            resume_before = int(paused_until, 16)
            if not resume_before:
                reason = "grant_revoked"
            elif resume_before > int(block["timestamp"], 16):
                reason = "grant_paused"
            else:
                reason = "paused_grant_expired"
        reasons.append(reason)
    if int(budget, 16) < int(args["amountWei"]):
        reasons.append("budget")
    result = {"status": "needs_authorization" if reasons else "within_grant",
            "reasons": reasons, "remainingWei": str(int(budget, 16)),
            "generation": str(int(generation, 16)), "blockHash": block["hash"]}

    if reasons:
        result["policy"] = {"recipient": "0x" + target[-40:], "callDataHash": "0x" + call_hash,
                            "expiresAt": int(expiry, 16), "blockTimestamp": int(block["timestamp"], 16),
                            "requestedWei": args["amountWei"],
                            "budgetShortfallWei": str(max(0, int(args["amountWei"]) - int(budget, 16)))}
        result["action"] = "Stop this purchase and ask the owner to review the grant."
    else:
        registry, scheme, version, key = read("0x95a8a93f" + agent_word, 4)
        if int(registry, 16):
            result.update(status="alternate_signature_required", registry="0x" + registry[-40:],
                          scheme="0x" + scheme, version=int(version, 16), keyCommitment="0x" + key,
                          action="Use the signer for this account's configured verification scheme.")
    return result


PREPARE = {**TOOL, "name": "prepare_purchase",
           "description": "Check authorization and return EIP-712 data for the isolated session signer."}
SIMULATE = {
    **TOOL, "name": "simulate_purchase",
    "description": "Simulate a signed purchase and return the transaction for a gas sponsor.",
    "inputSchema": {**TOOL["inputSchema"],
                    "properties": {**TOOL["inputSchema"]["properties"],
                                   "signature": {"type": "string", "pattern": "^0x[0-9a-fA-F]{130}$"}},
                    "required": [*TOOL["inputSchema"]["required"], "signature"]}}


def prepare_purchase(args, block=None):
    result = check_purchase(args, block)
    if result["status"] != "within_grant":
        return result
    account = os.environ["PGP_ACCOUNT"]
    chain_id = int(rpc("eth_chainId", []), 16)
    fields = [("agent", "address"), ("generation", "uint256"),
              ("taskId", "bytes32"), ("value", "uint256")]
    domain = [("chainId", "uint256"), ("verifyingContract", "address")]
    result["typedData"] = {
        "domain": {"chainId": str(chain_id), "verifyingContract": account},
        "primaryType": "Payment",
        "types": {name: [{"name": k, "type": v} for k, v in spec]
                  for name, spec in (("Payment", fields), ("EIP712Domain", domain))},
        "message": {"agent": os.environ["PGP_AGENT"], "generation": result["generation"],
                    "taskId": args["taskId"], "value": args["amountWei"]}}
    return result


def split_signature(signature):
    if not isinstance(signature, str) or not re.fullmatch("0x[0-9a-fA-F]{130}", signature):
        raise ValueError("invalid signature")
    recovery = int(signature[-2:], 16)
    if recovery in (0, 1):
        recovery += 27
    if recovery not in (27, 28):
        raise ValueError("invalid recovery id")
    return recovery, signature[2:66], signature[66:130]


def encode_call_bytes(args):
    raw = args.get("data", "0x")[2:]
    return format(len(raw) // 2, "064x") + raw.ljust((len(raw) + 63) // 64 * 64, "0")


def estimate_fee(transaction):
    gas = int(rpc("eth_estimateGas", [transaction]), 16)
    price = int(rpc("eth_gasPrice", []), 16)
    return gas, price


def preview_transaction(tx, block_hash, purchases):
    sponsor = os.environ.get("PGP_SPONSOR")
    estimation_tx = {**tx, "from": sponsor} if sponsor else tx
    rpc("eth_call", [estimation_tx, {"blockHash": block_hash, "requireCanonical": True}])
    gas, price = estimate_fee(estimation_tx)
    return {"status": "simulated", "transaction": tx, "blockHash": block_hash,
            "operations": [{"taskId": p["taskId"], "recipient": p["recipient"],
                            "nativeValueWei": p["amountWei"], "callData": p.get("data", "0x")}
                           for p in purchases],
            "nativeValueForwardedWei": str(sum(int(p["amountWei"]) for p in purchases)),
            "feeEstimate": {"payer": sponsor, "gas": str(gas), "gasPriceWei": str(price),
                            "feeWei": str(gas * price)}}


def simulate_purchase(args):
    recovery, r, s = split_signature(args.get("signature"))
    result = check_purchase({k: v for k, v in args.items() if k != "signature"})
    if result["status"] != "within_grant":
        return result
    words = [os.environ["PGP_AGENT"][2:], args["taskId"][2:],
             format(int(args["amountWei"]), "x"), format(7 * 32, "x"),
             format(recovery, "x"), r, s]
    tx = {"to": os.environ["PGP_ACCOUNT"], "data": "0xf959dd1b" + "".join(w.zfill(64) for w in words) + encode_call_bytes(args)}
    return preview_transaction(tx, result["blockHash"], [args])


RECEIPT = {
    "name": "purchase_receipt",
    "description": "Check payment inclusion and optionally query the node's finalized head.",
    "inputSchema": {"type": "object", "properties": {
        "txHash": {"type": "string", "pattern": "^0x[0-9a-fA-F]{64}$"},
        "taskId": {"type": "string", "pattern": "^0x[0-9a-fA-F]{64}$"},
        "includeFinality": {"type": "boolean"}},
        "required": ["txHash", "taskId"], "additionalProperties": False}}


def receipt_finality(receipt):
    try:
        finalized = rpc("eth_getBlockByNumber", ["finalized", False])
        canonical = rpc("eth_getBlockByNumber", [receipt["blockNumber"], False])
    except (NodeError, OSError):
        return "unknown"
    if canonical is None or canonical["hash"].lower() != receipt["blockHash"].lower():
        return "chain_changed"
    if finalized is None:
        return "unknown"
    if int(finalized["number"], 16) < int(receipt["blockNumber"], 16):
        return "awaiting_finality"
    if (int(finalized["number"], 16) == int(receipt["blockNumber"], 16)
            and finalized["hash"].lower() != receipt["blockHash"].lower()):
        return "chain_changed"
    return "finalized"


def purchase_receipt(args):
    keys = {"txHash", "taskId"}
    if (not keys <= args.keys() <= keys | {"includeFinality"}
            or not isinstance(args.get("includeFinality", False), bool)
            or not all(isinstance(args[k], str) and re.fullmatch("0x[0-9a-fA-F]{64}", args[k]) for k in keys)):
        raise ValueError("invalid receipt arguments")
    receipt = rpc("eth_getTransactionReceipt", [args["txHash"]])
    if receipt is None:
        return {"status": "awaiting_receipt"}
    details = {"blockHash": receipt["blockHash"], "txHash": args["txHash"],
               "taskId": args["taskId"], "payer": receipt.get("from")}
    if args.get("includeFinality"):
        details["finality"] = receipt_finality(receipt)
        if details["finality"] == "chain_changed":
            return {"status": "chain_changed", **details}
    if receipt.get("gasUsed") is not None and receipt.get("effectiveGasPrice") is not None:
        details["transactionExecutionFeeWei"] = str(int(receipt["gasUsed"], 16)
                                                    * int(receipt["effectiveGasPrice"], 16))
    if int(receipt["status"], 16) == 0:
        return {"status": "reverted", **details}
    for event in receipt["logs"]:
        topics = [t.lower() for t in event["topics"]]
        if (event["address"].lower() == os.environ["PGP_ACCOUNT"].lower()
                and len(topics) == 4 and not event.get("removed", False)
                and topics[0] == PAID_EVENT
                and topics[1] == args["taskId"].lower()
                and topics[2][-40:] == os.environ["PGP_AGENT"][2:].lower()):
            return {"status": "payment_included", **details,
                    "recipient": "0x" + topics[3][-40:], "amountWei": str(int(event["data"], 16))}
    return {"status": "no_matching_payment", **details}


SUBMIT = {**SIMULATE, "name": "submit_purchase",
          "description": "Recheck a session-signed purchase and submit through the configured gas sponsor."}


def submit_purchase(args):
    result = simulate_purchase(args)
    if result["status"] != "simulated":
        return result
    return sponsor_transaction(result["transaction"], {"taskId": args["taskId"]})


def sponsor_transaction(transaction, task_fields):
    sponsor = os.environ.get("PGP_SPONSOR", "")
    if not re.fullmatch("0x[0-9a-fA-F]{40}", sponsor):
        return {"status": "sponsor_not_configured"}
    tx = {**transaction, "from": sponsor}
    gas, price = estimate_fee(tx)
    ceiling = int(os.environ["PGP_SPONSOR_MAX_GAS"])
    if gas > ceiling or gas * price > int(os.environ["PGP_SPONSOR_MAX_FEE_WEI"]):
        return {"status": "sponsor_budget_exceeded"}
    tx.update(gas=hex(gas), gasPrice=hex(price))
    try:
        tx_hash = rpc("eth_sendTransaction", [tx])
    except (OSError, ValueError, KeyError, NodeError):
        # A lost response does not establish whether the node accepted the tx.
        return {"status": "submission_unconfirmed", **task_fields,
                "action": "Check settlement and the sponsor transaction history before retrying."}
    return {"status": "submitted", "txHash": tx_hash, **task_fields}


def batch_tool(name, item, description):
    return {"name": name, "description": description, "inputSchema": {
        "type": "object", "properties": {"purchases": {"type": "array", "minItems": 1,
            "maxItems": 32, "items": item["inputSchema"]}}, "required": ["purchases"],
        "additionalProperties": False}}


PREPARE_BATCH = batch_tool("prepare_purchase_batch", TOOL, "Check aggregate budget and prepare individual signatures.")
SUBMIT_BATCH = batch_tool("submit_purchase_batch", SIMULATE, "Simulate and submit the signed purchases atomically.")


def check_batch(args):
    items = args.get("purchases")
    if set(args) != {"purchases"} or not isinstance(items, list) or not 1 <= len(items) <= 32:
        raise ValueError("expected 1 to 32 purchases")
    tasks = [p["taskId"].lower() for p in items]
    if len(set(tasks)) != len(tasks):
        return {"status": "duplicate_task"}
    block = rpc("eth_getBlockByNumber", ["latest", False])
    requests = []
    for p in items:
        result = prepare_purchase(p, block)
        if result["status"] != "within_grant":
            return {**result, "taskId": p["taskId"]}
        requests.append(result["typedData"])
    if sum(int(p["amountWei"]) for p in items) > int(result["remainingWei"]):
        return {"status": "needs_authorization", "reasons": ["budget"]}
    return {"status": "within_grant", "typedData": requests, "blockHash": block["hash"]}


SIMULATE_BATCH = batch_tool("simulate_purchase_batch", SIMULATE, "Preview signed calls and the estimated total sponsor fee.")


def simulate_batch(args):
    batch = os.environ.get("PGP_BATCH", "")
    if not re.fullmatch("0x[0-9a-fA-F]{40}", batch):
        return {"status": "batch_not_configured"}
    if set(args) != {"purchases"} or not isinstance(args["purchases"], list):
        raise ValueError("invalid purchases")
    items = args["purchases"]
    result = check_batch({"purchases": [{k: v for k, v in p.items() if k != "signature"} for p in items]})
    if result["status"] != "within_grant":
        return result
    at = {"blockHash": result["blockHash"], "requireCanonical": True}
    if rpc("eth_getCode", [batch, at]) == "0x":
        raise ValueError("batch contract is missing")

    def word(n):
        return format(n, "064x")

    offsets, tuples = [], []
    offset = len(items) * 32
    for p in items:
        recovery, r, s = split_signature(p["signature"])
        # Payment tuple: six head words, followed by ABI-encoded call bytes.
        encoded = (p["taskId"][2:] + word(int(p["amountWei"])) + word(192)
                   + word(recovery) + r + s + encode_call_bytes(p))
        offsets.append(word(offset))
        tuples.append(encoded)
        offset += len(encoded) // 2
    head = "".join(os.environ[k][2:].zfill(64) for k in ("PGP_ACCOUNT", "PGP_AGENT")) + word(96)
    tx = {"to": batch, "data": "0x9f08efe2" + head + word(len(items)) + "".join(offsets + tuples)}
    return preview_transaction(tx, result["blockHash"], items)


def submit_batch(args):
    preview = simulate_batch(args)
    if preview["status"] != "simulated":
        return preview
    return sponsor_transaction(preview["transaction"], {"taskIds": [p["taskId"] for p in args["purchases"]]})


SIGN = {**TOOL, "name": "sign_purchase",
        "description": "Ask the isolated session signer to approve and sign one purchase intent."}


def request_signature(args):
    path = os.environ.get("PGP_SIGNER_SOCKET")
    if not path:
        return {"status": "signer_not_configured"}
    with socket.socket(socket.AF_UNIX) as client:
        client.settimeout(130)
        client.connect(path)
        client.sendall(json.dumps(args).encode() + b"\n")
        with client.makefile("rb") as stream:
            return json.loads(stream.readline(65536))


ACTIVITY = {
    "name": "review_purchase_activity",
    "description": "Inspect recent settled payments and flag transfers above an optional review threshold.",
    "inputSchema": {"type": "object", "additionalProperties": False, "properties": {
        "endBlock": {"type": "integer", "minimum": 0},
        "reviewAboveWei": {"type": "string", "pattern": "^[0-9]+$"}}}}


def review_activity(args):
    if set(args) - {"endBlock", "reviewAboveWei"}:
        raise ValueError("unexpected activity argument")
    end = args.get("endBlock")
    threshold = args.get("reviewAboveWei")
    if end is not None and (type(end) is not int or end < 0):
        raise ValueError("invalid end block")
    if threshold is not None and not re.fullmatch("[0-9]+", threshold):
        raise ValueError("invalid review threshold")
    account, agent = (os.environ[key] for key in ("PGP_ACCOUNT", "PGP_AGENT"))
    if not all(re.fullmatch("0x[0-9a-fA-F]{40}", addr) for addr in (account, agent)):
        raise ValueError("invalid configured account or agent")
    anchor = rpc("eth_getBlockByNumber", ["latest" if end is None else hex(end), False])
    if not anchor:
        return {"status": "block_unavailable"}
    end = int(anchor["number"], 16)
    start = max(0, end - 499)
    topic = "0x" + agent[2:].lower().zfill(64)
    logs = rpc("eth_getLogs", [{"address": account, "fromBlock": hex(start), "toBlock": hex(end),
                                "topics": [PAID_EVENT, None, topic]}])
    entries = []
    total = 0
    for log in logs:
        if log.get("removed"):
            continue
        topics = log["topics"]
        if (log["address"].lower() != account.lower() or len(topics) != 4
                or topics[0].lower() != PAID_EVENT or topics[2].lower() != topic):
            raise ValueError("unexpected payment log")
        value = int(log["data"], 16)
        total += value
        entries.append({"taskId": topics[1], "recipient": "0x" + topics[3][-40:],
                        "amountWei": str(value), "txHash": log["transactionHash"],
                        "blockNumber": int(log["blockNumber"], 16),
                        "aboveReviewThreshold": threshold is not None and value > int(threshold)})
    current = rpc("eth_getBlockByNumber", [hex(end), False])
    if not current or current["hash"] != anchor["hash"]:
        return {"status": "chain_changed", "action": "Repeat the activity query."}
    return {"status": "reviewed", "fromBlock": start, "toBlock": end,
            "endBlockHash": anchor["hash"], "previousEndBlock": start - 1 if start else None,
            "count": len(entries), "nativeValueForwardedWei": str(total), "payments": entries}


RESULT = {
    "name": "service_result",
    "description": "Read delivery status and commitments from the configured PgpResultInbox for this task.",
    "inputSchema": {"type": "object", "additionalProperties": False, "required": ["taskId"],
                    "properties": {"taskId": TOOL["inputSchema"]["properties"]["taskId"]}}}


def service_result(args):
    if set(args) != {"taskId"} or not re.fullmatch("0x[0-9a-fA-F]{64}", args["taskId"]):
        raise ValueError("invalid task identifier")
    inbox = os.environ.get("PGP_RESULT_INBOX")
    if not inbox:
        return {"status": "result_inbox_unconfigured"}
    if not re.fullmatch("0x[0-9a-fA-F]{40}", inbox):
        raise ValueError("invalid configured result inbox")
    block = rpc("eth_getBlockByNumber", ["latest", False])
    at = {"blockHash": block["hash"], "requireCanonical": True}
    # PgpResultInbox.results(bytes32) returns two flags and two commitments.
    raw = rpc("eth_call", [{"to": inbox, "data": "0x4c6b25b1" + args["taskId"][2:]}, at])
    if not re.fullmatch("0x(?:0{63}[01]){2}[0-9a-fA-F]{128}", raw):
        raise ValueError("unexpected result inbox response")
    requested, verified = raw[65] == "1", raw[129] == "1"
    if verified and not requested:
        raise ValueError("inconsistent result flags")
    report = {"taskId": args["taskId"], "inbox": inbox, "blockHash": block["hash"],
              "status": "unrequested"}
    if requested:
        report.update(status="awaiting_proof", inputCommitment="0x" + raw[130:194])
    if verified:
        report.update(status="result_verified", outputCommitment="0x" + raw[194:])
    return report


HANDLERS = {
    "service_result": service_result,
    "review_purchase_activity": review_activity,
    "check_purchase": check_purchase,
    "prepare_purchase": prepare_purchase,
    "sign_purchase": request_signature,
    "simulate_purchase": simulate_purchase,
    "submit_purchase": submit_purchase,
    "purchase_receipt": purchase_receipt,
    "prepare_purchase_batch": check_batch,
    "simulate_purchase_batch": simulate_batch,
    "submit_purchase_batch": submit_batch,
}


def dispatch(message):
    method = message.get("method")
    if method == "initialize":
        return {"protocolVersion": "2024-11-05", "capabilities": {"tools": {}},
                "serverInfo": {"name": "pgp-agent", "version": "0.1"}}
    if method == "tools/list":
        specs = [TOOL, PREPARE, SIMULATE, SUBMIT, RECEIPT, PREPARE_BATCH, SIMULATE_BATCH, SUBMIT_BATCH, SIGN, ACTIVITY, RESULT]
        read_only = {"service_result", "review_purchase_activity", "check_purchase", "prepare_purchase", "simulate_purchase",
                     "purchase_receipt", "prepare_purchase_batch", "simulate_purchase_batch"}
        return {"tools": [{**spec, "annotations": {"readOnlyHint": spec["name"] in read_only}}
                          for spec in specs]}
    if method == "ping":
        return {}
    if method == "tools/call" and message["params"]["name"] in HANDLERS:
        try:
            output = HANDLERS[message["params"]["name"]](message["params"]["arguments"])
            failed = False
        except NodeError as exc:
            output = {"status": "node_rejected_request", "method": exc.method, "code": exc.code}
            failed = True
        except OSError:
            output = {"status": "service_unreachable",
                      "action": "Check the configured node or signer connection."}
            failed = True
        except Exception as exc:
            output = {"status": "check_failed", "error": type(exc).__name__}
            failed = True
        return {"content": [{"type": "text", "text": json.dumps(output)}], "isError": failed}
    raise ValueError("unsupported method")


if __name__ == "__main__":
    for line in sys.stdin:
        message = {}
        try:
            message = json.loads(line)
            if "id" not in message:
                continue
            reply = {"result": dispatch(message)}
        except (ValueError, KeyError, TypeError):
            reply = {"error": {"code": -32600, "message": "Invalid request"}}
        print(json.dumps({"jsonrpc": "2.0", "id": message.get("id"), **reply}), flush=True)
