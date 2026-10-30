// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The pgp-chain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the pgp-chain library. If not, see <http://www.gnu.org/licenses/>.

package dpos

import (
	"fmt"

	dp2p "github.com/elastos/Elastos.ELA/dpos/p2p"
	"github.com/elastos/Elastos.ELA/dpos/p2p/hub"
	"github.com/elastos/Elastos.ELA/utils/elalog"

	glog "github.com/ethereum/go-ethereum/log"
)

// The DPoS TCP layer logs through elalog, and elalog's default is NO output:
// dpos/p2p/log.go calls DisableLog() from its init, so until somebody calls
// UseLogger every single message from that package is discarded -- including the
// ones that explain why two producers never connected.
//
// That default is not an oversight upstream, it is a library convention (btcd
// does the same), and upstream satisfies it by calling dpos.InitLog from the
// pbft constructor. This port deliberately dropped InitLog's file/rotation half
// (see log.go), which left the other half missing too. The consequence is
// specific and bad: `service magic %d not found` (hub.go:320) and
// `hostConnManger ip %s count %d OverMaxNodePerHost` (server.go:591) are logged
// at Debug, so a producer whose peer set refuses every connection produces a log
// file that looks completely healthy.
//
// p2pLogAdapter routes elalog straight into go-ethereum's root logger instead of
// giving elalog its own Backend over an io.Writer. Both work, but the adapter
// keeps the level information: a writer would see preformatted lines and could
// not tell a Warn from a Debug, so a magic mismatch would either be promoted to
// a visible level (noise on every healthy connect attempt) or demoted with the
// rest.
//
// Level always reports Debug and SetLevel does nothing on purpose. geth's root
// logger already applies --verbosity, and letting elalog filter as well would
// mean two verbosity settings that have to agree -- the exact thing log.go argues
// against for the rest of the dpos package.
type p2pLogAdapter struct{}

func (p2pLogAdapter) Debugf(format string, params ...interface{}) {
	glog.Debug(p2pLogPrefix + fmt.Sprintf(format, params...))
}
func (p2pLogAdapter) Infof(format string, params ...interface{}) {
	glog.Info(p2pLogPrefix + fmt.Sprintf(format, params...))
}
func (p2pLogAdapter) Warnf(format string, params ...interface{}) {
	glog.Warn(p2pLogPrefix + fmt.Sprintf(format, params...))
}
func (p2pLogAdapter) Errorf(format string, params ...interface{}) {
	glog.Error(p2pLogPrefix + fmt.Sprintf(format, params...))
}

// Fatal and Fatalf map to Error, NOT to glog.Crit.
//
// elalog's Fatal does not terminate the process: (*slog).Fatal only prints at
// "FAT" when the level allows it (Elastos.ELA/utils/elalog/log.go:361-375). This
// is unlike btcd, whose Fatal does exit, and unlike go-ethereum's Crit, which
// calls os.Exit(1) (log/logger.go:235-238). Mapping elalog.Fatal onto Crit would
// therefore turn a log line into a process exit -- inventing a crash the ported
// code never had. Nothing under dpos/p2p calls Fatal today (verified: zero
// occurrences), so this is about not planting the trap for the first call site
// that does.
func (p2pLogAdapter) Fatalf(format string, params ...interface{}) {
	glog.Error(p2pLogPrefix + "FATAL " + fmt.Sprintf(format, params...))
}
func (p2pLogAdapter) Fatal(v ...interface{}) {
	glog.Error(p2pLogPrefix + "FATAL " + fmt.Sprint(v...))
}

func (p2pLogAdapter) Debug(v ...interface{}) { glog.Debug(p2pLogPrefix + fmt.Sprint(v...)) }
func (p2pLogAdapter) Info(v ...interface{})  { glog.Info(p2pLogPrefix + fmt.Sprint(v...)) }
func (p2pLogAdapter) Warn(v ...interface{})  { glog.Warn(p2pLogPrefix + fmt.Sprint(v...)) }
func (p2pLogAdapter) Error(v ...interface{}) { glog.Error(p2pLogPrefix + fmt.Sprint(v...)) }

func (p2pLogAdapter) Level() elalog.Level   { return elalog.LevelDebug }
func (p2pLogAdapter) SetLevel(elalog.Level) {}

// p2pLogPrefix marks the origin. geth's lines carry no package name, and this
// package is the only one in the tree that speaks elalog, so without a prefix a
// reader cannot tell "the TCP dial failed" from "the engine refused to seal" --
// which are the two halves of every peer problem and are debugged differently.
const p2pLogPrefix = "[dpos-p2p] "

// InitP2PLog enables logging for the DPoS TCP layer.
//
// It must be called before dpos.NewNetwork, and only by a node that is going to
// run one: the packages it touches are process-global, and a verifier that calls
// it would get DPoS chatter from a subsystem it never started.
//
// hub needs its own call. p2p.UseLogger only reaches dtime, addrmgr, connmgr and
// peer; the hub keeps a separate logger, and the hub is exactly where the magic
// comparison and the "not in peers list" rejection happen.
func InitP2PLog() {
	logger := p2pLogAdapter{}
	dp2p.UseLogger(logger)
	hub.UseLogger(logger)
}
