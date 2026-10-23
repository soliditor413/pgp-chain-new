package spv

import (
	"fmt"

	elaspv "github.com/elastos/Elastos.ELA.SPV/interface"
	elalog "github.com/elastos/Elastos.ELA/utils/elalog"

	"github.com/ethereum/go-ethereum/log"
)

// elaLogAdapter implements elalog.Logger by forwarding to geth's logger.
//
// elalog.Logger is a printf-style interface, geth's takes a message plus
// key/value context, so each call is formatted first and handed over as the
// message.
//
// The adapter filters on its own min level rather than leaving it to geth,
// because the two have different notions of what is noisy: the ELA sync package
// logs one Info line per main chain block ("Received block ... at height N"),
// which is 2.3M lines for a full sync. Attaching at Warn keeps peer and indexing
// failures while dropping that trace.
type elaLogAdapter struct {
	tag string
	min elalog.Level
}

// enabled mirrors elalog's own rule (log.go:275 "if lvl <= LevelDebug"): a
// message is printed when its level is at or above the minimum.
func (a *elaLogAdapter) enabled(l elalog.Level) bool { return a.min <= l }

func (a *elaLogAdapter) prefix(format string, params ...interface{}) string {
	return "[" + a.tag + "] " + fmt.Sprintf(format, params...)
}

func (a *elaLogAdapter) join(v ...interface{}) string {
	return "[" + a.tag + "] " + fmt.Sprint(v...)
}

func (a *elaLogAdapter) Debugf(format string, params ...interface{}) {
	if a.enabled(elalog.LevelDebug) {
		log.Root().Debug(a.prefix(format, params...))
	}
}

func (a *elaLogAdapter) Infof(format string, params ...interface{}) {
	if a.enabled(elalog.LevelInfo) {
		log.Root().Info(a.prefix(format, params...))
	}
}

func (a *elaLogAdapter) Warnf(format string, params ...interface{}) {
	if a.enabled(elalog.LevelWarn) {
		log.Root().Warn(a.prefix(format, params...))
	}
}

func (a *elaLogAdapter) Errorf(format string, params ...interface{}) {
	if a.enabled(elalog.LevelError) {
		log.Root().Error(a.prefix(format, params...))
	}
}

// Fatalf maps to geth's Crit, which terminates the process — the same thing
// elalog's Fatalf does.
func (a *elaLogAdapter) Fatalf(format string, params ...interface{}) {
	log.Root().Crit(a.prefix(format, params...))
}

func (a *elaLogAdapter) Debug(v ...interface{}) {
	if a.enabled(elalog.LevelDebug) {
		log.Root().Debug(a.join(v...))
	}
}

func (a *elaLogAdapter) Info(v ...interface{}) {
	if a.enabled(elalog.LevelInfo) {
		log.Root().Info(a.join(v...))
	}
}

func (a *elaLogAdapter) Warn(v ...interface{}) {
	if a.enabled(elalog.LevelWarn) {
		log.Root().Warn(a.join(v...))
	}
}

func (a *elaLogAdapter) Error(v ...interface{}) {
	if a.enabled(elalog.LevelError) {
		log.Root().Error(a.join(v...))
	}
}

func (a *elaLogAdapter) Fatal(v ...interface{}) { log.Root().Crit(a.join(v...)) }

func (a *elaLogAdapter) Level() elalog.Level     { return a.min }
func (a *elaLogAdapter) SetLevel(l elalog.Level) { a.min = l }

var _ elalog.Logger = (*elaLogAdapter)(nil)

// AttachElaLogs routes the ELA SPV sub-package logs into geth's logger at the
// given minimum level.
//
// The ELA packages ship with logging switched off — interface/log.go calls
// DisableLog() from its init — so out of the box a node syncing the ELA main
// chain reports absolutely nothing about it. That is the wrong default here,
// because every ELA read on the execution path answers from this data and "did
// the deposit filter match anything" is the first question an operator will
// have. pgp-chain wrote these logs to a separate ./logs-spv/ directory with its
// own level from spvconfig.json; forwarding them into geth's logger instead means
// --verbosity, --log.json and rotation apply to them, and one file holds the
// whole story.
//
// Pass elalog.LevelInfo to see the per-block trace while debugging a sync that is
// not advancing. That is rarely needed: this package logs its own coverage
// progress through geth directly (BlockListener.NotifyBlock, every 10,000 ELA
// blocks), so a stalled sync is visible at the default verbosity anyway.
func AttachElaLogs(level elalog.Level) {
	elaspv.UseLogger(&elaLogAdapter{tag: "ELA-SPV", min: level})
}
