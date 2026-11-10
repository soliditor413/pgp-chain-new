package main

import (
	"path/filepath"
	"sync"

	elacom "github.com/elastos/Elastos.ELA/common"
	elalog "github.com/elastos/Elastos.ELA/utils/elalog"
	"github.com/urfave/cli/v2"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/spv"
)

// spvServiceHolder carries the running ELA SPV service across the two places
// that need it: the node lifecycle, which has to be registered in makeFullNode
// because RegisterLifecycle panics ("can't register lifecycle on running/stopped
// node") once the node is up, and startSpv, which creates the service before the
// node starts.
//
// A package level holder is acceptable here for the same reason the client
// identifier is: geth runs one node per process, and the SPV service is a
// singleton of that node.
var spvHolder = new(spvServiceHolder)

type spvServiceHolder struct {
	mu  sync.Mutex
	svc *spv.Service
}

// Start is a no-op: startSpv starts the service itself. It cannot be deferred to
// the lifecycle, because the service has to be syncing — and, more importantly,
// its recharge store has to be installed — before the eth/64 syncer executes the
// first legacy block, which happens as soon as the node starts.
func (h *spvServiceHolder) Start() error { return nil }

// Stop is the part that matters. The SPV service owns two leveldb stores — the
// ELA headers and the recharge index — and leaving them to the OS means an
// unclean header store and a half-written coverage marker on every restart.
func (h *spvServiceHolder) Stop() error {
	h.mu.Lock()
	svc := h.svc
	h.svc = nil
	h.mu.Unlock()

	if svc != nil {
		svc.Stop()
		spv.Close()
	}
	return nil
}

func (h *spvServiceHolder) set(svc *spv.Service) {
	h.mu.Lock()
	h.svc = svc
	h.mu.Unlock()
}

// elaLogLevel parses --spv.ela.loglevel.
//
// A bad value is fatal rather than silently replaced by the default: this flag is
// the one switch that makes the ELA library talk, and an operator who mistyped it
// would otherwise sit in front of exactly the silent stall they were trying to
// diagnose, believing they had turned the logging on.
func elaLogLevel(ctx *cli.Context) elalog.Level {
	raw := ctx.String(utils.SpvElaLogLevelFlag.Name)
	level, ok := elalog.LevelFromString(raw)
	if !ok {
		utils.Fatalf("Invalid %s %q: want one of debug, info, warn, error",
			utils.SpvElaLogLevelFlag.Name, raw)
	}
	return level
}

// startSpv starts the ELA main chain SPV service, which is where every piece of
// ELA data used during execution comes from.
//
// It is the port of pgp-chain's startSpv (cmd/pgp/main.go:398-507). What it does:
//
//   - derives the ELA main chain address that this chain's deposits are paid to,
//     from the genesis block hash. That single address is the whole filter: with
//     FilterType = FTReturnSidechainDepositCoinFilter the ELA peers then send us
//     the blocks' cross chain outputs paying it, i.e. exactly the deposits that
//     back the 0x3f1 getMainChainRechargeData precompile, rather than the main
//     chain's entire transaction set. Get it wrong and the filter matches
//     nothing, so every deposit block fails with ErrRechargeUnavailable.
//   - opens the recharge store (SpvDbInit), which also installs the real 0x3f1
//     precompile in place of the always-failing stub.
//   - starts the service, which connects to the ELA mainnet seeds, syncs headers
//     from ELA height 0 and indexes every block into the recharge store through
//     BlockListener.NotifyBlock. Nothing here trusts a peer's claim about a
//     transaction: the transactions arrive as merkle proofs against headers this
//     node synced itself, which is why there is no RPC fallback on this path.
//
// Only a pgp-chain has an ELA side to follow, so a node running any other chain
// config skips this entirely.
//
// testnet comes from --chapel: the pgp test network, whose EVM genesis is
// pgp-chain's testnet and whose ELA side is the ELA test chain. The mapping is a
// flag rather than a chain-config field because --chapel already means "this is a
// test deployment" to every operator here, and a second switch would let the two
// disagree. It is still checked against the chain config below, so a future chain
// that has no legacy section reports the flag as unused instead of silently
// running without an ELA side.
func startSpv(stack *node.Node, backend ethapi.Backend, elaLevel elalog.Level, ctx *cli.Context) {
	bc := backend.Chain()
	if bc == nil {
		return
	}

	elaNet := spv.NetMainnet
	if ctx.IsSet(utils.ChapelFlag.Name) {
		elaNet = spv.NetTestnet
	}

	if !bc.Config().IsPGPLegacy(nil) {
		// Reached whenever the chain has no legacy (PBFT) era. Saying so matters
		// for --chapel: without this line a testnet node starts, connects, syncs,
		// and simply never has an ELA side, and the operator's only clue is an
		// absence.
		if ctx.IsSet(utils.ChapelFlag.Name) {
			log.Warn("--chapel given but this chain has no legacy config; the ELA SPV service is not started",
				"chainID", bc.Config().ChainID)
		}
		return
	}

	genesis := bc.Genesis()
	if genesis == nil {
		utils.Fatalf("Cannot start the ELA SPV service: the chain has no genesis block")
	}
	depositAddress, err := spv.CalculateGenesisAddress(genesis.Hash().String())
	if err != nil {
		utils.Fatalf("Cannot derive the ELA deposit address from genesis %s: %v", genesis.Hash(), err)
	}

	// The ELA stores live next to the chain data so --datadir and the network
	// subdirectory apply to them too.
	dir := filepath.Join(stack.InstanceDir(), "spv")

	// Stamp the directory with the ELA network before anything opens a database
	// in it. Two networks' headers in one store is not a clean failure: the store
	// is keyed by hash and records no chain identity, so the mixture surfaces as
	// forks and arbiter sets that make no sense.
	if err := spv.EnsureNetworkDir(dir, elaNet); err != nil {
		utils.Fatalf("Cannot use the ELA SPV data directory: %v", err)
	}

	log.Info("Starting the ELA SPV service",
		"dataDir", dir, "genesis", genesis.Hash(),
		"elaNet", elaNet, "elaDepositAddress", depositAddress)

	// The ELA sub-packages log nothing until a logger is attached, and the level
	// is what decides whether a refused block explains itself; see
	// spv.AttachElaLogs and --spv.ela.loglevel.
	spv.AttachElaLogs(elaLevel)

	spv.SpvDbInit(dir, "", [20]byte{}, nil)

	// dynamicArbiterHeight is the ELA height from which the listener starts
	// maintaining the next-turn DPOS arbiter set. pgp-chain takes it from the
	// chain config, which SetEthConfig filled in from --spv.arbiter.height
	// (cmd/pgp/main.go:456-463); the mainnet value is 2075778.
	//
	// This used to be math.MaxUint64, i.e. "never", on the theory that the
	// next-turn bookkeeping only feeds the ELA dpos p2p / broadcast write path
	// and could therefore be skipped. That reasoning was wrong in one respect
	// worth recording: the same gate also guards the POW->DPOS transition in
	// onBlockHandled, so with MaxUint64 the listener reported
	// "l.dynamicArbiterHeight = 18446744073709551615" on every live block and
	// could never reach the code below it. The value is a mainnet constant, so
	// it belongs in the chain config next to the fee split and the pass
	// balance, not in the start-up path.
	dynamicArbiterHeight := bc.Config().DynamicArbiterHeight()

	// The 2075778 default is an ELA *mainnet* height. On a network whose chain is
	// shorter than that, waiting for it means the next-turn bookkeeping never
	// starts - and the only symptom is an ELA height that stops being useful, not
	// an error. Zero means "track from the beginning", which is the right answer
	// for a test network and is why the override is keyed on the chain config
	// having left the field unset rather than on the network alone: a testnet that
	// genuinely wants a specific height can still say so.
	if elaNet != spv.NetMainnet && bc.Config().Legacy.DynamicArbiterHeight == 0 {
		log.Warn("ELA testnet: overriding the mainnet next-turn DPOS height",
			"mainnetHeight", dynamicArbiterHeight, "using", 0,
			"reason", "the mainnet constant does not exist on this network")
		dynamicArbiterHeight = 0
	}

	// calculate ELA mainchain address from the genesis block hash and set the SPV monitor address accordingly
	genesisU256, err := elacom.Uint256FromBytes(genesis.Hash().Bytes())
	if err != nil {
		utils.Fatalf("Blockchain not running: %v", err)
	}
	svc, err := spv.NewService(&spv.Config{
		DataDir:        dir,
		GenesisAddress: depositAddress,
		ActiveNet:      elaNet,
		GenesisHash:    *genesisU256,
	}, new(event.TypeMux), dynamicArbiterHeight)
	if err != nil {
		utils.Fatalf("SPV service init error: %v", err)
	}

	log.Info("Starting the ELA SPV listener",
		"dynamicArbiterHeight", dynamicArbiterHeight,
		"note", "next-turn DPOS bookkeeping starts at this ELA height")

	spvHolder.set(svc)
	svc.Start()
}
