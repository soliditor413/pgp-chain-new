#!/usr/bin/env python3
"""seed-peers -- pre-seed each producer's DPoS address book.

WHY THIS EXISTS

A producer finds the other producers' DPoS listen addresses in one of two ways:

  1. Gossip. dpos.Routes signs a DAddr record (your --pbft.net.address and
     --pbft.net.port, encrypted to each peer's PID), broadcasts an Inv over
     devp2p ELAMSG (0x08), answers the resulting GetData, and the receiver
     decrypts it in OnCipherAddr -> Network.AddDirectLinkAddr.

  2. This script. It writes the same information straight into the address
     database the DPoS server loads at startup, so ConnectPeers has a dial
     target without any gossip having to happen.

Path 1 is the production one and it is implemented faithfully (dpos/routes.go is
a close copy of pgp-chain's). It now works end-to-end on this tree, which took
three fixes and is worth writing down because the failure was silent:

  - eth/p2p_dpos_peer.go announces peers from the devp2p layer too, not only from
    eth/64 (dpos.Routes needs the peer in its cache before it will answer anything).
  - eth64 is registered a second time as its own capability, "elamsg"/1
    (eth/protocols/eth64/protocol.go). It used to be "eth"/64 alongside eth/68 and
    eth/70, and p2p/peer.go's matchProtocols keys by capability name only, so the
    higher eth version replaced it on every connection: eth64's peer set stayed
    empty, BroadcastElaMsg iterated nothing, and no Inv ever left any node. Every
    DPoS producer showed NoneConnection/ip="unknown" with five healthy devp2p
    peers and not one line of error.
  - eth64's BestPeer skips peers whose version is not ETH64, so those elamsg
    connections are visible to BroadcastElaMsg/PeerByID without being handed to
    the syncer, which would send GetBlockHeaders into a window the far end
    discards.

Measured on the six-producer rig with every peers.json removed: DPoS network
started at t+0 with 0 addresses, "PBFT: producers connected, starting recovery
peers=4 need=4" at t+38s, first imported block at t+91s, activedpospeers=10 on
all six, six heads and anchors identical. So gossip alone is sufficient.

This script is still the fast path and the deterministic one: pre-seeded, the
same rig dials at startup instead of waiting ~40s for the address round trip, and
a node whose gossip is broken by a config mistake (wrong --pbft.net.port, magic,
advertised IP, firewall) still connects. It is also the discriminator: pre-seed
and the set connects -> the consensus stack is fine and the gossip is what needs
debugging; pre-seed and it still does not connect -> the problem is downstream of
addressing.

WHAT IT WRITES

    <datadir>/geth/network/dpos/peers.json      (mode 0600)

    {
      "Addresses": [
        {"PID": [33 byte values], "Addr": "host:port"},
        ...
      ]
    }

PID is the 33 byte compressed P-256 public key as an array of numbers, not hex
and not base64: dpos/p2p's addrmgr decodes peer.PID as a [33]byte, so a hex
string fails to unmarshal. Addr is where THAT producer listens on
--pbft.net.port, which is a different port from devp2p --port.

The DPoS data dir is <datadir>/geth/network/dpos because eth/backend.go sets
Config.Pbft.DataDir = stack.ResolvePath("network/dpos") and geth resolves
relative paths under <datadir>/geth.

USAGE

Run it with no arguments to see which rigs and manifests it can find.

  # A rig built by multinode.sh (has keys/producers.json and node*/node.env).
  # Everything is inferred: ports from DPOS_PORT, indices from the node.env
  # header, datadirs from the node* dirs.
  ./seed-peers.py --from-rig ~/.pgp-devnet-multi

  # Any other layout. node i is the node whose keystore is arbiter-<i>.dat,
  # i.e. the manifest's "index" field -- NOT its position in the sorted
  # producers array.
  ./seed-peers.py --manifest ./keys/producers.json \
      --node 127.0.0.1:20679:/data/n0 \
      --node 127.0.0.1:20680:/data/n1

  # Datadirs on different machines: run this once per host and name the node.
  ./seed-peers.py --manifest ./keys/producers.json \
      --node 10.0.0.1:20679 --node 10.0.0.2:20679 --node 10.0.0.3:20679 \
      --only 1 --datadir /data/n1

Stop the nodes first: the file is read once at server start, and the address
manager rewrites it on shutdown, so writing it under a running node is undone.
"""

import argparse
import glob
import ipaddress
import json
import os
import re
import sys

# Where a rig usually lives. Only used by the no-argument discovery mode; nothing
# here is required.
RIG_HINTS = [
    os.path.expanduser("~/.pgp-devnet-multi"),
    os.path.expanduser("~/.pgp-devnet"),
    os.path.expanduser("~/.pgp-legacy-private"),
]


def die(msg):
    print("ERROR: %s" % msg, file=sys.stderr)
    sys.exit(1)


def warn(msg):
    print("WARN: %s" % msg, file=sys.stderr)


# --- input parsing ----------------------------------------------------------

def parse_node(spec):
    """HOST:PORT[:DATADIR] -- split from the right, a datadir may contain colons."""
    rest, sep, datadir = spec.rpartition(":")
    if not sep:
        die("--node %r: want HOST:PORT[:DATADIR]" % spec)
    host, sep, port = rest.rpartition(":")
    if not sep:
        die("--node %r: want HOST:PORT[:DATADIR]" % spec)
    try:
        port = int(port)
    except ValueError:
        die("--node %r: port %r is not a number" % (spec, port))
    if not 0 < port < 65536:
        die("--node %r: port out of range" % spec)
    # An address the *other* producers can reach. 127.0.0.1 is correct only when
    # every node is on one host; on separate hosts it turns every dial into a
    # self-connection and the set never converges.
    try:
        ipaddress.ip_address(host)
    except ValueError:
        die("--node %r: host %r is not a literal IP address. A hostname works "
            "only if every node resolves it to the same reachable IP; use the "
            "literal address so the failure modes stay obvious." % (spec, host))
    return {"host": host, "port": port, "datadir": datadir or None}


def load_manifest(path):
    """Return index -> {hex, raw}, checking the shape addrmgr needs."""
    try:
        with open(path) as f:
            doc = json.load(f)
    except OSError as e:
        die("cannot read %s: %s" % (path, e))
    except ValueError as e:
        die("%s is not valid JSON: %s" % (path, e))

    accounts = doc.get("accounts")
    if not accounts:
        die("%s has no \"accounts\" array; it is not a manifest keygen writes" % path)

    by_index = {}
    for a in accounts:
        try:
            idx = int(a["index"])
            pub = a["publicKey"]
        except (KeyError, TypeError, ValueError):
            die("manifest account %r is missing index or publicKey" % (a,))
        key = pub[2:] if pub.startswith("0x") else pub
        try:
            raw = bytes.fromhex(key)
        except ValueError:
            die("manifest index %d: publicKey %r is not hex" % (idx, pub))
        # 33 bytes is the compressed P-256 point the ELA wallet emits. Anything
        # else unmarshals into a garbage PID and the dial silently goes nowhere.
        if len(raw) != 33:
            die("manifest index %d: publicKey is %d bytes, want 33 (compressed "
                "P-256). An uncompressed 65 byte key is the usual cause." % (idx, len(raw)))
        if raw[0] not in (2, 3):
            die("manifest index %d: publicKey starts with 0x%02x, want 0x02 or "
                "0x03 -- that prefix is what makes it a compressed point." % (idx, raw[0]))
        if idx in by_index:
            die("manifest has two accounts with index %d" % idx)
        by_index[idx] = {"hex": key.lower(), "raw": raw}

    return by_index


ENV_RE = re.compile(r"^([A-Z0-9_]+)='([^']*)'$", re.M)


def nodes_from_rig(rig):
    """Build the node list from a multinode.sh rig: keys/producers.json + node*/node.env."""
    manifest = os.path.join(rig, "keys", "producers.json")
    if not os.path.isfile(manifest):
        die("%s has no keys/producers.json -- not a rig built by 'multinode.sh init'" % rig)

    by_index = load_manifest(manifest)
    nodes = [None] * len(by_index)

    envs = sorted(glob.glob(os.path.join(rig, "node*", "node.env")))
    if not envs:
        die("%s has keys/producers.json but no node*/node.env -- pass --node flags "
            "and --datadir instead" % rig)

    for env_path in envs:
        node_dir = os.path.dirname(env_path)
        with open(env_path) as f:
            text = f.read()
        # The header carries the index: "# Generated by multinode.sh init. Node 3 of 4."
        m = re.search(r"Node (\d+) of (\d+)", text)
        if not m:
            die("%s has no 'Node N of M' header; cannot tell which producer it is" % env_path)
        idx = int(m.group(1))
        if idx >= len(nodes):
            die("%s says node %d but the manifest has %d accounts" % (env_path, idx, len(nodes)))
        if nodes[idx] is not None:
            die("two node.env files claim index %d" % idx)

        env = dict(ENV_RE.findall(text))
        port = env.get("DPOS_PORT")
        if not port:
            die("%s has no DPOS_PORT" % env_path)
        # multinode.sh puts every node on one host, so the advertised address is
        # loopback. Override with --host when that is not true.
        nodes[idx] = {"host": "127.0.0.1", "port": int(port), "datadir": node_dir}

    missing = [i for i, n in enumerate(nodes) if n is None]
    if missing:
        die("manifest index %s has no node*/node.env in %s" % (missing, rig))

    return by_index, nodes, manifest


# --- discovery --------------------------------------------------------------

def discovery_report():
    """No arguments: say what was found and print a command that would work."""
    print("seed-peers: no arguments given.\n")

    found = []
    for hint in RIG_HINTS:
        hint = os.path.abspath(hint)
        if os.path.isfile(os.path.join(hint, "keys", "producers.json")):
            found.append(hint)

    if not found:
        print("No rig with keys/producers.json found in the usual places:")
        for hint in RIG_HINTS:
            print("  %s" % os.path.abspath(hint))
        print("\nPoint it at yours, either way:")
        print("  %s --from-rig <rig dir>              # if it has node*/node.env"
              % os.path.basename(sys.argv[0]))
        print("  %s --manifest <producers.json> \\    # otherwise"
              % os.path.basename(sys.argv[0]))
        print("      --node HOST:PORT:DATADIR [--node ...]")
        return

    print("Rigs found (pass one with --from-rig):\n")
    for rig in found:
        manifest = os.path.join(rig, "keys", "producers.json")
        try:
            n = len(json.load(open(manifest))["accounts"])
        except (OSError, ValueError, KeyError):
            n = "?"
        envs = glob.glob(os.path.join(rig, "node*", "node.env"))
        print("  %s" % rig)
        print("      %s producers, %d node.env" % (n, len(envs)))
        if envs and n != "?" and len(envs) == n:
            print("      -> %s --from-rig %s --dry-run"
                  % (os.path.basename(sys.argv[0]), rig))
        else:
            print("      -> needs --node flags: %s producers vs %d node.env"
                  % (n, len(envs)))
    print("\nRun with --dry-run first: it prints every file it would write.")


# --- main -------------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser(
        description="Write network/dpos/peers.json into each producer's datadir. "
                    "Run with no arguments to see what a rig looks like.")
    ap.add_argument("--manifest", help="producers.json written by keygen, or any "
                                       "JSON with an accounts[] of {index, publicKey}")
    ap.add_argument("--from-rig", metavar="DIR",
                    help="a rig built by multinode.sh: reads keys/producers.json "
                         "and node*/node.env so no --node flags are needed")
    ap.add_argument("--node", action="append", default=[], metavar="HOST:PORT[:DATADIR]",
                    help="one per producer, ordered by the manifest's index field. "
                         "HOST:PORT is that node's --pbft.net.address/port.")
    ap.add_argument("--host", help="override the advertised host for --from-rig "
                                   "(default 127.0.0.1, correct only on one machine)")
    ap.add_argument("--datadir", help="datadir of the --only node, for when the "
                                      "datadirs are not all on this machine")
    ap.add_argument("--only", type=int, metavar="INDEX",
                    help="write only this node's file. Use it when one host holds "
                         "one producer and the others are on other machines.")
    ap.add_argument("--dry-run", action="store_true", help="print, do not write")
    ap.add_argument("--force", action="store_true", help="overwrite an existing peers.json")
    args = ap.parse_args()

    if not args.manifest and not args.from_rig and not args.node:
        discovery_report()
        return

    if args.from_rig:
        if args.node:
            die("--from-rig and --node are alternatives; --from-rig infers the nodes")
        by_index, nodes, manifest_path = nodes_from_rig(os.path.expanduser(args.from_rig))
        if args.host:
            for n in nodes:
                n["host"] = args.host
    else:
        if not args.manifest:
            die("--manifest is required unless you pass --from-rig")
        by_index = load_manifest(os.path.expanduser(args.manifest))
        manifest_path = args.manifest
        nodes = [parse_node(s) for s in args.node]
        if not nodes:
            die("no --node given (or use --from-rig for a multinode.sh rig)")

    # --node order is by index, so a gap would hand one lucky node another's
    # address: 5 --node flags against a 6 account manifest would shift every
    # port by one.
    if len(nodes) != len(by_index):
        die("%d node(s) but %s has %d accounts; need exactly one per producer"
            % (len(nodes), manifest_path, len(by_index)))
    missing = sorted(set(by_index) - set(range(len(nodes))))
    if missing:
        die("manifest index %s has no node (they must be 0..%d, contiguous)"
            % (missing, len(nodes) - 1))

    # Two producers on one host:port means the second bind fails and its peers
    # dial whoever won. Catch it here rather than in the log.
    seen = {}
    for i, n in enumerate(nodes):
        key = (n["host"], n["port"])
        if key in seen:
            die("node %d and node %d both claim %s:%d" % (seen[key], i, n["host"], n["port"]))
        seen[key] = i

    if args.only is not None:
        if not 0 <= args.only < len(nodes):
            die("--only %d is out of range (0..%d)" % (args.only, len(nodes) - 1))
        if not args.datadir:
            die("--only needs --datadir: the other nodes' datadirs are not here")
        nodes[args.only]["datadir"] = args.datadir
        targets = [args.only]
    else:
        targets = list(range(len(nodes)))

    for i in targets:
        n = nodes[i]
        if not n["datadir"]:
            die("node %d has no datadir; add ':DATADIR' to its --node, or use --only "
                "with --datadir on the host that holds it" % i)
        if not os.path.isdir(n["datadir"]):
            die("node %d: datadir %s does not exist" % (i, n["datadir"]))

        target_dir = os.path.join(n["datadir"], "geth", "network", "dpos")
        target = os.path.join(target_dir, "peers.json")

        addrs = []
        for j, other in enumerate(nodes):
            if j == i:
                continue  # never list yourself: the server would dial loopback
            addrs.append({
                "PID": list(by_index[j]["raw"]),
                "Addr": "%s:%d" % (other["host"], other["port"]),
            })

        blob = json.dumps({"Addresses": addrs}, indent=2) + "\n"

        print("node %d  %s:%d  -> %s  (%d peers)"
              % (i, n["host"], n["port"], target, len(addrs)))
        for j, other in enumerate(nodes):
            if j == i:
                continue
            print("    %-21s pid=%s..." % ("%s:%d" % (other["host"], other["port"]),
                                           by_index[j]["hex"][:16]))

        if args.dry_run:
            continue
        if os.path.exists(target) and not args.force:
            print("    SKIP: exists (use --force to replace)")
            continue
        if not os.path.isdir(os.path.dirname(target_dir)):
            # <datadir>/geth is created by `geth init`; if it is missing the
            # datadir is wrong, not merely empty.
            warn("%s has no geth/ subdir -- is the datadir right? creating it anyway"
                 % n["datadir"])
        os.makedirs(target_dir, mode=0o700, exist_ok=True)
        with open(target, "w") as f:
            f.write(blob)
        # 0600: not because the contents are secret (PIDs are public, ports are
        # not sensitive) but because a forged entry would redirect every
        # consensus message, and per-node state should not be writable by others.
        os.chmod(target, 0o600)

    if args.dry_run:
        print("\nDry run: nothing written. Re-run without --dry-run.")
    else:
        print("\nDone. Restart each node: the address book is read once at start.")


if __name__ == "__main__":
    main()
