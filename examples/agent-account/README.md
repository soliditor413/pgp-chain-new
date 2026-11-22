# PGP agent purchases

A service-buying agent can request native-currency payments against a grant from
the wallet owner. The account stores a target, exact calldata hash, native wei
budget, expiry (Unix seconds), and generation for each session key. Omitting data selects an ordinary payment. Supply optional hex data to call
an approved contract, including a zero-wei call. The grant must bind the exact
calldata hash and the signer policy must approve those same bytes. ERC-20 amounts require a token-specific policy.

## Connect an agent

Compile PgpAgentAccount.sol with Solidity 0.8.26 and an EVM target supported
by the chain (Paris was used in the example test). Deploy with the owner's
address, fund the account, and call authorize from the owner wallet. Pass the
agent's signer address, service recipient, keccak256 of empty bytes,
budget in wei, and expiry. To stop future purchases call revoke(agent).

From the repository root, using a running private PGP devnet and an existing
Foundry owner keystore, the deployment commands are:

```sh
cd examples
forge build
forge create --rpc-url "$PGP_RPC_URL" --account "$OWNER_KEYSTORE" --broadcast \
  agent-account/PgpAgentAccount.sol:PgpAgentAccount --constructor-args "$OWNER_ADDRESS"
```

Set PGP_ACCOUNT to the deployed address printed by Forge. Send test native
currency to that address from the devnet owner wallet. In the owner console
enter that same address and grant the session key a budget. PGP_RPC_URL must
match the wallet network (the four-node local test uses port 18560 on node 0).

Register this stdio server with your MCP client, replacing the path, RPC and
addresses. The core tools run on Python 3 using only the standard library.

```json
{
  "mcpServers": {
    "pgp": {
      "command": "python3",
      "args": ["/absolute/path/to/pgp-c/examples/agent-account/agent_tools.py"],
      "env": {
        "PGP_RPC_URL": "http://127.0.0.1:8545",
        "PGP_ACCOUNT": "<deployed account address>",
        "PGP_AGENT": "<session signer address>"
      }
    }
  }
}
```

## Buy a service

1. Assign a stable 32-byte taskId to the purchase. Supply the same recipient,
   amountWei (decimal string), and taskId on retries.
2. Call check_purchase or prepare_purchase. Stop on already_paid. Show the
   reasons for needs_authorization to the owner. Only the owner can change
   the grant.
3. Pass the returned typedData to a separate session signer. Before signing,
   the signer must check its configured chain ID, account, session address,
   current grant generation, target, operation, amount, and task against
   the intent approved by the user. Keep the private key outside model
   inputs and tool results.
4. Call simulate_purchase with the original arguments and the 65-byte
   signature. Give the returned transaction to a gas sponsor. In a private
   development node with a node-managed sponsor, submit_purchase can perform
   this step when PGP_SPONSOR, PGP_SPONSOR_MAX_GAS and PGP_SPONSOR_MAX_FEE_WEI
   are set. The fee ceiling applies per transaction.
5. Call purchase_receipt with txHash and taskId. awaiting_receipt means the
   transaction has no receipt at the queried node. payment_included reports
   the matching event, amount, recipient and block hash. Apply the application's
   finality policy before releasing irreversible service results. Track API
   delivery and quality separately in the service.

The tools support MCP stdio discovery and calls. They do not host a language
model. They can be used with an existing MCP-capable agent runtime.

## Settle several purchases together

Deploy PgpPurchaseBatch and configure its address as PGP_BATCH in the server.
Send an array named purchases to prepare_purchase_batch. Each entry has the
same fields as check_purchase. Use distinct task IDs. The tool validates the
whole budget at one canonical block and returns one typed message per item.
After the session signer approves each item, add its signature to that item
and pass the array to submit_purchase_batch. This uses the configured sponsor
and its transaction fee limits. All payments settle together. Query each
task with purchase_receipt using the same transaction hash.

## Connect a session signer

The optional sign_purchase MCP tool talks to a local policy service. Run
session_signer.py under a trusted OS identity. Give the agent access only to
the purchase socket, with a restricted shared group or ACL if using separate
identities. The signer creates its socket with owner-only access by default.
Keep the policy, Clef IPC socket, session keystore and signer environment
outside the agent's write access.

Start Clef with the session keystore and the intended chain ID. Configure its
approval UI according to the owner's policy. For the Clef shipped with this repository, --ipcpath accepts a directory.
From the repository root start Clef in a separate terminal, for example:

```sh
./build/bin/clef --keystore "$SESSION_KEYSTORE_DIR" --chainid 860621 \
  --configdir "$CLEF_CONFIG_DIR" --ipcpath "$CLEF_IPC_DIR"
```

Use the socket at `$CLEF_IPC_DIR/clef.ipc` in a signer-owned JSON file:

```json
{
  "chainId": 860621,
  "account": "<account contract>",
  "agent": "<session key address>",
  "clefIpc": "/absolute/path/to/clef.ipc",
  "tasks": {
    "<lowercase 0x-prefixed 32-byte task ID>": {
      "recipient": "<service address>", "amountWei": "80", "generation": 1
    }
  }
}
```

Set PGP_ACCOUNT, PGP_AGENT and PGP_RPC_URL independently in the signer's
environment, then run
`python3 session_signer.py --socket /private/run/path/purchases.sock --policy /absolute/path/policy.json`.
Configure PGP_SIGNER_SOCKET in the MCP server with that purchase socket.
The agent calls sign_purchase with the original purchase arguments. On each
request the signer rereads the owner's policy, queries the chain and reconstructs
the typed payment before asking Clef to sign. The generation must match the
grant created by the owner. Return the signature to submit_purchase.

## Review a signed operation

Call simulate_purchase or simulate_purchase_batch before submission. The
preview lists the target, exact calldata, task ID and native wei forwarded for
each call. feeEstimate gives estimated gas units, gas price and the sponsor's
estimated fee in wei. The forwarded native value excludes token transfers or
other internal effects of arbitrary contracts. Fees and chain state may change
between simulation and inclusion. Submission rechecks the grant and fee caps.

## Open the owner console

Serve this directory locally with
`python3 -m http.server 8080 --bind 127.0.0.1` and open
`http://127.0.0.1:8080/console.html` in a browser with an EVM wallet.
Enter the expected chain ID, account contract and agent. Direct owner wallets
can issue grants, pause, resume and revoke. For a council-owned account, submit
owner actions through the council's vote and execute workflow. The spending
history groups matching Paid events by account and agent in 500-block windows.

### Authorize recurring native purchases

For a service with a fixed recipient, the owner can let the session signer
approve new task IDs without adding each purchase to the policy file. Add
this field to the signer-owned policy (alongside the chain, account, session
address and Clef IPC path):

```json
"nativePurchaseSession": {
  "recipient": "<service address>",
  "maxPerPurchaseWei": "80",
  "generation": 1
}
```

Each purchase must have positive native value, empty calldata, the approved
recipient, and a fresh task ID. The signer checks the current on-chain grant
before signing. Its remaining budget and expiry apply across the session;
the account enforces the aggregate budget even if signed purchases arrive
concurrently. Explicit entries in tasks take precedence for those task IDs.
Renewing or resuming a grant changes its generation. Update the signer policy
only after the owner approves the new generation.

For a delivery that requires final settlement, pass includeFinality: true to
purchase_receipt. Wait for payment_included and finality equal to finalized.
An unavailable finalized head produces unknown. A mismatched canonical block
returns chain_changed; look up the transaction again before releasing the service.
