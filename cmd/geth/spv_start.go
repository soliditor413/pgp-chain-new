package main

import (
	"path/filepath"
	"sync"

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
func startSpv(stack *node.Node, backend ethapi.Backend, elaLevel elalog.Level) {
	bc := backend.Chain()
	if bc == nil || !bc.Config().IsPGPLegacy(nil) {
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
	log.Info("Starting the ELA SPV service",
		"dataDir", dir, "genesis", genesis.Hash(), "elaDepositAddress", depositAddress)

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

	svc, err := spv.NewService(&spv.Config{
		DataDir:        dir,
		GenesisAddress: depositAddress,
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
