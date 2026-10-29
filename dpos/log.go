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
	"strings"

	glog "github.com/ethereum/go-ethereum/log"
)

// pgp-chain's dpos package logs through a dedicated rotating file logger
// (initLog, pgp-chain/dpos/log.go) configured with its own directory, level and
// rotation limits. This port drops that: the node already has one logging
// pipeline (go-ethereum's, with rotation handled by the shell that runs it), and
// a second one means two verbosity settings, two rotation policies and a log
// file operators forget to look at.
//
// The helper names are kept so the ~300 ported call sites stay line-for-line
// reviewable against the original. They funnel into go-ethereum's root logger,
// which means `--verbosity` and `--log.json` apply to consensus chatter too.

// sprintArgs joins one call's operands the way fmt.Sprintln does, then drops the
// single trailing newline Sprintln appends.
//
// Sprint would be the obvious choice and it is wrong: Sprint only inserts spaces
// between operands when neither side is a string, so Sprint("is self", true)
// renders "is self true" but Sprint("viewOffset", "3") renders "viewOffset3".
// The ported call sites pass bare identifiers as arguments all over the place
// (`Info("is self", self)`), so Sprint silently fused them into one word and made
// the logs unreadable -- a format regression that no compiler and no test could
// see. Sprintln separates every operand unconditionally, which is what these
// call sites were written against.
//
// It is kept as a separate function rather than inlined into the four helpers so
// the formatting rule is testable: the helpers themselves are one-line wrappers
// around glog, and a test for them has to reach through the logger.
func sprintArgs(a ...interface{}) string {
	return strings.TrimSuffix(fmt.Sprintln(a...), "\n")
}

func Debug(a ...interface{}) { glog.Debug(sprintArgs(a...)) }

func Info(a ...interface{}) { glog.Info(sprintArgs(a...)) }

func Warn(a ...interface{}) { glog.Warn(sprintArgs(a...)) }

func Error(a ...interface{}) { glog.Error(sprintArgs(a...)) }

func Debugf(format string, a ...interface{}) { glog.Debug(fmt.Sprintf(format, a...)) }

func Infof(format string, a ...interface{}) { glog.Info(fmt.Sprintf(format, a...)) }

func Warnf(format string, a ...interface{}) { glog.Warn(fmt.Sprintf(format, a...)) }

func Errorf(format string, a ...interface{}) { glog.Error(fmt.Sprintf(format, a...)) }
