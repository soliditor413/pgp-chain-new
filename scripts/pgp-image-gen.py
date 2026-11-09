#!/usr/bin/env python3
"""Build the PGP system-contract image from bsc-genesis-contract's forge artifacts.

The chain's genesis is already live, so the alloc cannot be edited to place these
contracts. They are injected by protocol code at Legacy.SystemContractDeployHeight
instead, and that code needs the runtime bytecode as a Go asset. This script is
the only sanctioned way to produce that asset: it extracts deployedBytecode from
the fork's build output, proves the artifacts are current (not stale), and writes
a manifest whose imageHash is a canonical-JSON keccak over the entries.

Determinism is a hard requirement: the output contains no timestamps, absolute
paths, hostnames or build ids, so running this twice on the same inputs produces
byte-identical files. `--check` proves it against what is on disk.

Exit codes: 0 ok, 1 any gate failed (the failing contract is named), 2 usage.
"""

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys

# The 17 accounts. Address comes from core/systemcontracts/const.go (the const
# name is recorded so this table can be checked against that file rather than
# trusted); source is relative to the bsc-genesis-contract root.
#
# deployInit is "this contract's one-shot initializer runs at deploy time, in the
# protocol, from coinbase with gasPrice 0". StakeCredit is false on purpose: its
# initialize(address,string) is called by StakeHub once per validator, later.
ACCOUNTS = [
    ("0x0000000000000000000000000000000000001000", "ValidatorContract",          "BSCValidatorSet",       "contracts/BSCValidatorSet.sol",                    True),
    ("0x0000000000000000000000000000000000001001", "SlashContract",              "SlashIndicator",        "contracts/SlashIndicator.sol",                     True),
    ("0x0000000000000000000000000000000000001002", "SystemRewardContract",       "SystemReward",          "contracts/SystemReward.sol",                       False),
    ("0x0000000000000000000000000000000000001003", "LightClientContract",        "TendermintLightClient", "contracts/deprecated/TendermintLightClient.sol",   True),
    ("0x0000000000000000000000000000000000001004", "TokenHubContract",           "TokenHub",              "contracts/TokenHub.sol",                           True),
    ("0x0000000000000000000000000000000000001005", "RelayerIncentivizeContract", "RelayerIncentivize",    "contracts/deprecated/RelayerIncentivize.sol",      True),
    ("0x0000000000000000000000000000000000001006", "RelayerHubContract",         "RelayerHub",            "contracts/deprecated/RelayerHub.sol",              True),
    ("0x0000000000000000000000000000000000001007", "GovHubContract",             "GovHub",                "contracts/GovHub.sol",                             False),
    ("0x0000000000000000000000000000000000001008", "TokenManagerContract",       "TokenManager",          "contracts/deprecated/TokenManager.sol",            False),
    ("0x0000000000000000000000000000000000002000", "CrossChainContract",         "CrossChain",            "contracts/deprecated/CrossChain.sol",              True),
    ("0x0000000000000000000000000000000000002001", "StakingContract",            "Staking",               "contracts/deprecated/Staking.sol",                 False),
    ("0x0000000000000000000000000000000000002002", "StakeHubContract",           "StakeHub",              "contracts/StakeHub.sol",                           True),
    ("0x0000000000000000000000000000000000002003", "StakeCreditContract",        "StakeCredit",           "contracts/StakeCredit.sol",                        False),
    ("0x0000000000000000000000000000000000002004", "GovernorContract",           "BSCGovernor",           "contracts/BSCGovernor.sol",                        True),
    ("0x0000000000000000000000000000000000002005", "GovTokenContract",           "GovToken",              "contracts/GovToken.sol",                           True),
    ("0x0000000000000000000000000000000000002006", "TimelockContract",           "BSCTimelock",           "contracts/BSCTimelock.sol",                        True),
    ("0x0000000000000000000000000000000000003000", "TokenRecoverPortalContract", "TokenRecoverPortal",    "contracts/TokenRecoverPortal.sol",                 True),
]

# 0x2007 PaymentLane is deliberately absent: contracts/ and out/ have no source
# for it, the only copy in the bsc repo is an unattributed hex blob under
# jenner/mainnet/, and the user decided not to ship it in this image.

SOLC_BY_VERSION = {"000604": "0.6.4", "000811": "0.8.17"}

EMPTY_KECCAK = "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"


class GateError(Exception):
    pass


def keccak_hex(hexstr):
    """keccak256 of the BYTES hexstr encodes, as a 0x-prefixed hex string."""
    out = subprocess.run(["cast", "keccak", "0x" + hexstr], capture_output=True, text=True)
    if out.returncode != 0:
        raise GateError("cast keccak failed: " + out.stderr.strip())
    return out.stdout.strip()


def keccak_utf8(text):
    """keccak256 of the UTF-8 bytes of text."""
    out = subprocess.run(["cast", "keccak", text], capture_output=True, text=True)
    if out.returncode != 0:
        raise GateError("cast keccak failed: " + out.stderr.strip())
    return out.stdout.strip()


def keccak_selftest():
    """The whole manifest rests on one assumption: cast hashes bytes for 0x…
    and UTF-8 for everything else. Prove it with the empty string, whose hash is
    a fixed constant, instead of trusting it. A wrong assumption here would make
    every codeHash wrong while the Go test (same broken input) still agreed."""
    if shutil.which("cast") is None:
        raise GateError("cast not found in PATH: cannot compute keccak256 (python has no sha3/eth_hash here, "
                        "and sha256 is not a substitute)")
    got = keccak_utf8("")
    if got != "0x" + EMPTY_KECCAK:
        raise GateError(f"keccak self-test failed: keccak(\"\") = {got}, want 0x{EMPTY_KECCAK}; "
                        f"cast is not hashing what this script assumes")
    # And the 0x form must agree with the UTF-8 form on the same (empty) bytes.
    if keccak_hex("") != "0x" + EMPTY_KECCAK:
        raise GateError("keccak self-test failed: keccak(0x) != keccak(\"\") for the empty input")


def canonical_json(value):
    """Canonical serialisation for imageHash (design doc §7.4).

    Keys sorted, no whitespace, UTF-8, arrays keep their order, and integers are
    emitted VERBATIM as JSON numbers (unquoted) -- that is plain compact JSON, so
    any language's stdlib reproduces it. Booleans are true/false literals; this
    image has no floats and the spec forbids them.

    Only the entries array is ever canonicalised: provenance (sourceCommit /
    dirtyDiffSha256 / foundry) is excluded so the same image built from a
    different git state keeps the same hash.
    """
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, int):
        return str(value)
    if isinstance(value, str):
        return json.dumps(value, ensure_ascii=False)
    if value is None:
        return "null"
    if isinstance(value, list):
        return "[" + ",".join(canonical_json(v) for v in value) + "]"
    if isinstance(value, dict):
        return "{" + ",".join(json.dumps(k, ensure_ascii=False) + ":" + canonical_json(v)
                             for k, v in sorted(value.items())) + "}"
    raise GateError(f"canonical_json: unsupported type {type(value)}")


def load_artifact(out_dir, name):
    path = os.path.join(out_dir, name + ".sol", name + ".json")
    if not os.path.exists(path):
        raise GateError(f"{name}: artifact missing ({path}); run forge build in bsc-genesis-contract")
    with open(path) as f:
        return path, json.load(f)


def extract_bytecode(name, art_path, art):
    """G1/G2/G3: structural gates on the artifact's deployedBytecode."""
    dep = art.get("deployedBytecode") or {}
    obj = dep.get("object") or ""

    # G1: no unresolved library links.
    if dep.get("linkReferences"):
        raise GateError(f"{name}: deployedBytecode has linkReferences {dep['linkReferences']}")
    if "__" in obj:
        raise GateError(f"{name}: deployedBytecode contains a link placeholder ('__')")

    h = obj[2:] if obj.startswith("0x") else obj
    # G2: valid, plausible hex.
    if len(h) % 2 != 0:
        raise GateError(f"{name}: hex length {len(h)} is odd")
    if not re.fullmatch(r"[0-9a-f]*", h):
        raise GateError(f"{name}: deployedBytecode is not lowercase hex")
    code_len = len(h) // 2
    if code_len < 100:
        raise GateError(f"{name}: codeLen {code_len} < 100, that is not a contract")

    # G3: solc metadata trailer proves which compiler produced it and that the
    # ipfs hash is off (bytecode_hash='none'). Layout at the end:
    #   a1 64 736f6c63 43 <3 bytes version> 00 0a
    raw = bytes.fromhex(h)
    if raw[-2:] != b"\x00\x0a":
        raise GateError(f"{name}: metadata trailer does not end with 000a (got {raw[-2:].hex()})")
    if raw[-12:-6] != bytes.fromhex("a164736f6c63") or raw[-6] != 0x43:
        raise GateError(f"{name}: metadata trailer has no a1 64 'solc' 43 prefix (got {raw[-12:].hex()})")
    ver = raw[-5:-2].hex()
    if ver not in SOLC_BY_VERSION:
        raise GateError(f"{name}: unexpected solc version bytes 0x{ver} (allowed {sorted(SOLC_BY_VERSION)})")
    if "69706673" in raw[-64:].hex():  # "ipfs"
        raise GateError(f"{name}: metadata still carries an ipfs hash; foundry.toml expects bytecode_hash='none'")

    return h, code_len, SOLC_BY_VERSION[ver]


def check_freshness(name, art_path, source_path, foundry_toml):
    """G4: a stale artifact would silently ship the wrong code."""
    art_m = os.path.getmtime(art_path)
    if not os.path.exists(source_path):
        raise GateError(f"{name}: source missing ({source_path})")
    src_m = os.path.getmtime(source_path)
    if art_m < src_m:
        raise GateError(f"{name}: artifact is OLDER than its source "
                        f"({art_path} < {source_path}); rebuild before generating")
    if os.path.exists(foundry_toml) and art_m < os.path.getmtime(foundry_toml):
        raise GateError(f"{name}: artifact is older than foundry.toml; rebuild before generating")


def read_consts(const_go):
    """G5: read the addresses out of const.go instead of re-typing them here."""
    with open(const_go) as f:
        text = f.read()
    found = dict(re.findall(r'(\w+)\s*=\s*"(0x[0-9a-fA-F]{40})"', text))
    return {k: v.lower() for k, v in found.items()}


def init_signature_and_selector(name, art):
    """G7: the init selector must come from the ABI, not from a comment."""
    candidates = []
    for a in art.get("abi") or []:
        if a.get("type") == "function" and a.get("name") in ("init", "initialize"):
            inputs = ",".join(i["type"] for i in a.get("inputs") or [])
            candidates.append(f"{a['name']}({inputs})")
    if not candidates:
        return "", ""
    # At most one initializer per contract; more than one means the ABI is not
    # what this script assumes.
    if len(candidates) > 1:
        raise GateError(f"{name}: ABI has several initializers {candidates}")
    sig = candidates[0]
    sel = keccak_utf8(sig)[:10]
    cast_sig = subprocess.run(["cast", "sig", sig], capture_output=True, text=True)
    if cast_sig.returncode != 0:
        raise GateError(f"{name}: cast sig failed: {cast_sig.stderr.strip()}")
    if cast_sig.stdout.strip() != sel:
        raise GateError(f"{name}: keccak({sig}) = {sel} but cast sig says {cast_sig.stdout.strip()}")
    return sig, sel


def build(out_dir, contracts_root, bsc_root):
    keccak_selftest()

    const_go = os.path.join(bsc_root, "core/systemcontracts/const.go")
    consts = read_consts(const_go)
    foundry_toml = os.path.join(contracts_root, "foundry.toml")

    entries = []
    files = {}
    for address, const_name, name, source, deploy_init in ACCOUNTS:
        # G5: address table agrees with const.go.
        have = consts.get(const_name)
        if have is None:
            raise GateError(f"{name}: const.go has no {const_name}")
        if have != address:
            raise GateError(f"{name}: const.go {const_name} = {have}, table says {address}")

        art_path, art = load_artifact(out_dir, name)
        check_freshness(name, art_path, os.path.join(contracts_root, source), foundry_toml)
        code_hex, code_len, solc = extract_bytecode(name, art_path, art)
        sig, sel = init_signature_and_selector(name, art)
        if name == "BSCValidatorSet" and sel != "0x1663df00":
            raise GateError(f"BSCValidatorSet: init selector {sel}, want 0x1663df00 -- the F-1 ABI fix is "
                            f"not in this artifact")

        entries.append({
            "address": address,
            "name": name,
            "source": source,
            "solc": solc,
            "codeLen": code_len,
            "codeHash": keccak_hex(code_hex),
            "initSignature": sig,
            "initSelector": sel,
            "deployInit": deploy_init,
        })
        files[name] = code_hex

    # The image's own hash: canonical JSON of the entries only.
    image_hash = keccak_utf8(canonical_json(entries))

    head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=contracts_root,
                          capture_output=True, text=True, check=True).stdout.strip()
    dirty = subprocess.run(["git", "status", "--porcelain"], cwd=contracts_root,
                           capture_output=True, text=True, check=True).stdout
    diff = subprocess.run(["git", "diff", "HEAD"], cwd=contracts_root,
                          capture_output=True, check=True).stdout
    # An exact tag on HEAD is what a reviewer actually searches for; the commit
    # alone forces them to run `git describe`. Empty when HEAD is untagged.
    describe = subprocess.run(["git", "describe", "--tags", "--exact-match", "HEAD"],
                              cwd=contracts_root, capture_output=True, text=True)

    manifest = {
        "version": 1,
        "sourceRepo": "github.com/pgprotocol/bsc-genesis-contract",
        "sourceCommit": head,
        "sourceTag": describe.stdout.strip() if describe.returncode == 0 else "",
        "sourceDirty": bool(dirty.strip()),
        "dirtyDiffSha256": "0x" + hashlib.sha256(diff).hexdigest(),
        "foundry": {"optimizer": True, "optimizerRuns": 200, "bytecodeHash": "none"},
        "entries": entries,
        "imageHash": image_hash,
    }
    return manifest, files


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    bsc_root = os.path.dirname(here)

    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--artifacts", default=os.path.join(os.path.dirname(bsc_root), "bsc-genesis-contract", "out"),
                    help="foundry out/ directory of bsc-genesis-contract")
    ap.add_argument("--out", default=os.path.join(bsc_root, "core/systemcontracts/pgp"),
                    help="image package directory (default: core/systemcontracts/pgp)")
    ap.add_argument("--check", action="store_true",
                    help="regenerate in memory and compare with what is on disk; exit 1 on any difference")
    args = ap.parse_args()

    out_dir = os.path.abspath(args.artifacts)
    contracts_root = os.path.dirname(out_dir)

    try:
        manifest, files = build(out_dir, contracts_root, bsc_root)
    except GateError as e:
        print(f"GATE FAILED: {e}", file=sys.stderr)
        return 1

    image_dir = os.path.join(args.out, "image")
    manifest_path = os.path.join(args.out, "manifest.json")

    if args.check:
        problems = []
        for name, code in sorted(files.items()):
            p = os.path.join(image_dir, name)
            if not os.path.exists(p):
                problems.append(f"missing image/{name}")
                continue
            with open(p) as f:
                if f.read() != code:
                    problems.append(f"image/{name} differs")
        with open(manifest_path) as f:
            on_disk = f.read()
        want = json.dumps(manifest, indent=2) + "\n"
        if on_disk != want:
            problems.append("manifest.json differs")
        if problems:
            print("CHECK FAILED:\n  " + "\n  ".join(problems), file=sys.stderr)
            return 1
        print(f"check ok: 17 entries, imageHash {manifest['imageHash']}")
        return 0

    os.makedirs(image_dir, exist_ok=True)
    for name, code in files.items():
        with open(os.path.join(image_dir, name), "w") as f:
            f.write(code)  # bare lowercase hex, no 0x, no newline
    with open(manifest_path, "w") as f:
        f.write(json.dumps(manifest, indent=2) + "\n")

    total = sum(e["codeLen"] for e in manifest["entries"])
    print(f"wrote {len(manifest['entries'])} entries, {total} bytes of bytecode")
    print(f"imageHash {manifest['imageHash']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
