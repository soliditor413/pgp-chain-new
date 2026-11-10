# PGP system-contract image

Runtime bytecode for the **17** system contracts the protocol installs when the
chain switches to Parlia, plus a manifest that pins where every byte came from.

```
image/<Name>      bare lowercase hex, no 0x, no newline  (17 files)
manifest.json     commit + dirty-diff hash + foundry settings + per-entry metadata
image.go          //go:embed, load-time verification, Entries/Code/ImageHash
scripts/pgp-image-gen.py   the only sanctioned producer (repo root)
```

## Why an image at all

The pgp-chain genesis is live, so its alloc cannot be edited to place these
contracts. They are installed by protocol code at
`Legacy.SystemContractDeployHeight` — `SetCode` plus `SetNonce(addr, 1)` for each
account — and that code needs the runtime bytecode as a Go asset. This package is
that asset, kept free of any state writes itself: it only answers "what code, at
which address, initialised how".

## Provenance: this is NOT upstream v1.2.6

Source: `github.com/pgprotocol/bsc-genesis-contract`, commit
`24c7f2b54e388a9e2fa7a3dbc8b0ff2555b56817`, tag **`pgp-fork-v4`** — the fork's
own history on top of upstream v1.2.6 (`041881a`). Successive pins, so a reader
can tell what came from where:

| Tag | Commit | What it added | Contracts that moved |
|---|---|---|---|
| `pgp-fork-v1` | `5d2d047` | the fork's work committed (F-04) | 8 vs upstream |
| `pgp-fork-v2` | `6895b8c` | `onlyCoinbase onlyZeroGasPrice` on the 0.6.4 initialisers, 255-seat cap (F-03) | 7 |
| `pgp-fork-v3` | `b6071a1` | `not initialized` on the consensus read paths (F-08); `returndatasize` checks on both precompile calls (F-11b) | 3 |
| `pgp-fork-v4` | `24c7f2b` | `init` stores the BLS vote address instead of parsing and dropping it (F-22) | 1 |

So the bytecode here is **byte-for-byte different from the upstream tag**, by
design. `manifest.json` records exactly where it came from:

- `sourceCommit` / `sourceTag` — the commit and its tag. `sourceDirty` must be
  **false**: the bytes are reproducible from that commit, and
  `TestImageProvenanceIsCommitted` fails if anyone commits an image built from a
  dirty tree;
- `dirtyDiffSha256` — the sha256 of `git diff HEAD` at build time. With a clean
  tree it is the sha256 of the empty string, which is what it is now;
- per-entry `solc` — the image spans **two solc majors**: 11 contracts built with
  `0.6.4` (evmVersion istanbul) and 6 with `0.8.17` (london), all with
  optimizer enabled / 200 runs, `bytecode_hash = 'none'`.

The two-major span matters to an auditor asking "was this one build": it was one
`forge build`, but not one compiler.

## Regenerating

From the bsc repo root, with `bsc-genesis-contract` checked out at
`pgp-fork-v4` next to it and built:

```
git -C ../bsc-genesis-contract checkout pgp-fork-v4
forge build --force --root ../bsc-genesis-contract
python3 scripts/pgp-image-gen.py --artifacts ../bsc-genesis-contract/out
python3 scripts/pgp-image-gen.py --artifacts ../bsc-genesis-contract/out --check   # exits 0 if identical
python3 scripts/pgp-image-gen.py --artifacts ../bsc-genesis-contract/out           # idempotence: git status must stay clean
```

`node_modules` has to be installed there too (`npm install`): the OpenZeppelin
imports resolve through it, and it is not vendored.

The output contains no timestamps, absolute paths or hostnames, so the same
inputs produce byte-identical files. This was verified end to end: a clean
worktree of `pgp-fork-v4`, `forge build --force`, and the regenerated image
matched all 17 files byte for byte.

## Gates the generator enforces (G1–G7)

Any failure exits non-zero and names the contract; nothing is written.

| gate | what it proves |
|---|---|
| **G1** link placeholders | `deployedBytecode.linkReferences` empty and no `__` in the object — an unlinked artifact is not deployable code |
| **G2** hex validity | even length, `[0-9a-f]` only, `codeLen = len/2`, `codeLen >= 100` |
| **G3** metadata trailer | ends `a1 64 "solc" 43 <version> 00 0a`, version is `0x000604` (0.6.4) or `0x000811` (0.8.17 — the patch byte is 0x11 = 17), and no `ipfs` marker in the last 64 bytes (`bytecode_hash = 'none'`) |
| **G4** freshness | artifact mtime ≥ its `.sol` mtime **and** ≥ `foundry.toml` mtime — the "stale artifact" trap |
| **G5** address cross-check | the 17 addresses are read out of `core/systemcontracts/const.go` and compared, so a typo in the account table cannot ship |
| **G6** determinism | no timestamps/paths/hostnames in the output; `--check` and a second run must both be clean |
| **G7** selector self-proof | `initSelector = keccak(signature)[:4]` recomputed from the ABI and cross-checked with `cast sig`; `BSCValidatorSet` must be exactly `0x1663df00` (the `init(bytes,uint256)` ABI without which Parlia refuses to start) |

`cast` availability is not assumed either: the script first proves keccak on the
empty input (`keccak("") == c5d24601…a470`) before hashing anything, because a
wrong assumption there would make all 17 codeHashes wrong while the Go side
agreed with itself.

## 0x2007 PaymentLane is deliberately missing

`bsc-genesis-contract@041881a` has neither a source nor an artifact for it —
`contracts/` does not contain the file. The only copy in the bsc repo is
`core/systemcontracts/jenner/mainnet/PaymentLaneContract`, an unattributed hex
blob kept for the Jenner upgrade path. Putting it in this image would attach
provenance to bytes nobody can regenerate, so the image ships 17 accounts and the
deployment code must treat 0x2007 as "not in this image".

## Why StakeCredit has `deployInit: false`

Its initializer is `initialize(address,string)` **`onlyStakeHub`**: it is called
by StakeHub once per validator, when that validator's credit contract is created
— not at deploy time. Calling it from the protocol at the deploy height would
fail the modifier. The other `deployInit: true` contracts have zero-argument
initializers whose `initializer onlyCoinbase onlyZeroGasPrice` modifiers the
protocol's own call satisfies (`from = coinbase`, `gasPrice = 0`).

## What is verified at load time

`image.go`'s `init` decodes every image file and panics unless `keccak(code)`
equals the manifest's `codeHash`, `len(code)` equals `codeLen`, the init selector
equals `keccak(initSignature)[:4]`, and no address repeats. Consensus data that
is wrong must fail at startup, not produce a state root that quietly differs from
every other node's.

The check that the address **set** equals `core/systemcontracts/const.go`'s
constants lives in `image_test.go` (external package `pgp_test`) rather than in
`init`, because the deployment code in that parent package will import this one,
and an import in both directions is a cycle. The external test package asserts
the same property without that constraint.

`imageHash` is `keccak256(UTF8(canonicalJSON(entries)))` — keys sorted, no
whitespace, integers emitted verbatim as JSON numbers (plain compact JSON, not a
custom "numbers as strings" dialect), arrays in order. Provenance fields are
excluded, so the same image built from a different git state keeps the same
hash. `image_test.go` recomputes it with an encoder written independently of both
the generator and `image.go`, and pins the expected literal so any encoding drift
turns red immediately.

Design rationale (deploy height, initialisation order, what is still open) lives
in `docs/design/pgp-system-contract-deploy-design.md`; this file documents the
asset only.
