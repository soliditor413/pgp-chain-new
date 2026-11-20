#!/bin/bash
set -e

BSC_CONFIG=${BSC_HOME}/config/config.toml
BSC_GENESIS=${BSC_HOME}/config/genesis.json

# Init genesis state if the chain data directory does not exist yet.
# /pgp is the node instance directory (node/config.go: instanceDir = datadir/<clientIdentifier>).
DATA_DIR=$(cat ${BSC_CONFIG} | grep -A1 '\[Node\]' | grep -oP '\"\K.*?(?=\")')

PGP_DIR=${DATA_DIR}/pgp
if [ ! -d "$PGP_DIR" ]; then
  pgp --datadir ${DATA_DIR} init ${BSC_GENESIS}
fi

exec "pgp" "--config" ${BSC_CONFIG} "$@"
