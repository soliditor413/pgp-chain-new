# Verifier deployment and key migration

Deploy PgpAccountCouncil with independent controller addresses and a nonzero
delay. Pick a quorum attainable after one controller key is lost if recovery
from that loss is required. Set the council as governor of PgpProofRegistry
and owner of a new PgpAgentAccount at deployment. For each change, members
call vote with identical target, calldata, and salt. Once quorum and delay
are satisfied, any caller can execute those exact arguments. Use a new salt
for a new proposal. The council can rotate a lost controller key through a
proposal targeting itself. Rotation invalidates all outstanding proposals.

## Account and session keys

1. Evaluate the candidate signature scheme, public-key representation, audited
   implementation, worst-case verification gas and signature size before use.
2. Deploy an immutable verifier implementing IPgpProofVerifier. The adapter
   must check the signature with the public key whose commitment was authorized.
   For example, an adapter can accept public key, payment digest and signature
   in its proof envelope, recompute the statement below, and verify the digest.
   A scheme ID identifies the algorithm, serialization, and commitment rules.
3. Register its scheme and version with a gas limit through council approval.
   Versions are single-use. Code hash pinning alone cannot pin a proxy's
   implementation or mutable dependencies; use separately deployed immutable
   verifiers for reproducible validation.
4. The owner calls setSignaturePolicy for the agent. This advances the grant
   generation while preserving remaining budget, target, calldata hash and
   expiry. Wait for inclusion, then use the new generation when signing.
5. Compute the payment digest using account.digest. The verifier receives
   keccak256(abi.encode(keyCommitment, paymentDigest)) as its public statement.
   Submit the scheme-specific signature envelope with payWithSignature.
   Check the Paid event and settled(taskId) through the usual receipt workflow.
6. Disable a compromised version at the registry and move affected accounts
   to an approved replacement. Returning to the legacy ECDSA route requires
   explicit owner authorization with a zero-valued policy. Each switch consumes
   a generation, including a switch back.

Clef and the bundled MCP signing tool implement the ECDSA path. A new algorithm
also needs a compatible off-chain signer, transaction encoder and measured
execution costs. Existing deployed immutable account bytecode needs its own
migration procedure; a source update changes subsequent deployments only.

## Community proofs

Applications can use the same registry independently of the account. Define
the statement to bind the chain, application, action, public inputs and a
replay-prevention identifier where required. Retain witnesses off-chain and
provide only the proof and necessary public inputs. Check verify's boolean
result before settlement. Registered and Disabled events identify verifier
versions for indexers. Ordinary EVM gas pays for calldata, verification and
storage. The per-version limit bounds the gas forwarded to the verifier.

## Validator and governance migration

Parlia routes the existing aggregate finality proof through
`verifyAttestationProof` with the BLS v1 rule selected by its legacy header layout.
The caller derives voters and quorum from the canonical snapshot. A new rule
needs a consensus activation condition and a matching decoder for its key and
signature formats. Compare verification time and allocations for 4, 21 and 64
signers with `go test ./consensus/parlia -run TestAttestationProofVersion -bench BenchmarkAttestationProof -benchmem`.
This benchmark covers key decoding and aggregate verification, excluding block
execution, network transport and signature generation.

The legacy P-256 producer keys, Parlia block seals and BLS attestations travel
through distinct consensus and networking paths. A production algorithm change
needs versioned key registration, epoch activation, serialization and peer
compatibility, replay across the fork boundary, finality and recovery tests,
and measurements with a representative validator set. The account contract's
migration changes none of those consensus formats. Council members may vote directly or enroll an ERC-1271 member wallet.
A relayer submits that wallet's versioned signature through relayVote. The ballot
binds the council and chain, proposal ID (including council epoch), support,
member nonce and deadline. Direct and relayed votes both consume the member's
nonce. Each member wallet governs its own verification and recovery. Quorum
and activation delay apply to both routes. No post-quantum algorithm has been selected.

PgpResultInbox demonstrates the application side for an off-chain computation.
The requester records a salted input commitment for a unique task. The service
submits an output commitment and proof to that task's inbox; the signed or proven
statement binds the chain, inbox, scheme, version, task, input and output. Query
the Verified event by task ID to find accepted results. Store the witness and
any encrypted payload in the application's chosen off-chain storage. The scheme
must implement the service-specific validity rules. The regression fixture uses
a signature attestation to test routing and replay; it provides no zero-knowledge
or confidentiality guarantee.
