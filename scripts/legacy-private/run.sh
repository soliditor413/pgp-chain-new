#!/usr/bin/env bash
#
# Legacy (pgp-chain PBFT/DPoS) private chain on the bsc client.
#
# This directory is the *configuration* half of a private chain: a genesis whose
# config.legacy block reproduces the semantics of a pgp-chain private genesis.
# All the machinery (Elastos arbiter keygen, producer wiring, start/stop/status)
# already lives in scripts/pbft-devnet and is reused verbatim -- the only thing
# that changes here is which genesis template gets rendered.
#
#   ./run.sh init      # arbiter keystore + rendered genesis + geth init
#   ./run.sh start     # producing node (foreground, Ctrl-C stops it)
#   ./run.sh status    # eth_blockNumber
#   ./run.sh stop      # SIGTERM the running node from another terminal
#   ./run.sh clean     # delete keys, genesis, datadir, logs
#
# WHY 1 PRODUCER BY DEFAULT
#   The confirm threshold is int(N*2/3) down, so N=1 needs 0 votes and one node
#   finalises alone; N=4 needs 2 and N=6 needs 4, i.e. those sizes will sit at
#   height 0 forever on a single node. Raise PRODUCERS only together with the
#   matching number of running nodes:
#       PRODUCERS=4 ./run.sh init   # then bring up 2+ of the 4 keystores
#
# NOTHING here touches the repository tree: keys, genesis, datadir and logs go
# to $PGP_DEVNET_HOME (default ~/.pgp-legacy-private).

set -euo pipefail

SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SELF_DIR/../.." && pwd)
DEVNET_RUN=$REPO_ROOT/scripts/pbft-devnet/run.sh

[ -f "$DEVNET_RUN" ] || { printf 'ERROR: missing %s\n' "$DEVNET_RUN" >&2; exit 1; }

export GENESIS_TEMPLATE=${GENESIS_TEMPLATE:-$SELF_DIR/genesis.json}
export PGP_DEVNET_HOME=${PGP_DEVNET_HOME:-$HOME/.pgp-legacy-private}
# Must match genesis.json's config.chainId: it is also the p2p network id.
export CHAIN_ID=${CHAIN_ID:-83}
export PRODUCERS=${PRODUCERS:-1}
# Distinct from pbft-devnet's 8555/30399 so both can run side by side.
export HTTP_PORT=${HTTP_PORT:-8577}
export P2P_PORT=${P2P_PORT:-30401}
export GETH_BIN=${GETH_BIN:-$REPO_ROOT/build/bin/geth}

exec "$DEVNET_RUN" "$@"
