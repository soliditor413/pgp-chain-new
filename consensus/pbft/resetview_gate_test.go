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

package pbft

import "testing"

// TestStuckChainDoesNotTriggerAResetRequestPerTick is the reset storm, as a test.
//
// Measured on the six-producer chain at 22:51: 8900 to 9500 resetConsensus calls
// per node in fifteen minutes, about 10Hz per node, six nodes at once, and not one
// round completed in that time. The trigger was `vo >= maxViewOffset` against an
// offset that the anchor repair had just turned into a function of the clock: the
// chain stopped at height 5, the anchor froze at that block's timestamp, and vo
// grew without bound -- 249 within fourteen minutes. So the comparison was true on
// every tick of every node for as long as the chain stayed stuck, and each true
// answer broadcast a request and voted for its own request.
//
// The assertion that matters is the one about a node that comes up on an
// already-stuck chain: it must say nothing at all until it has personally watched
// maxViewOffset whole views go by. It did not watch the earlier ones fail, and a
// large offset is not evidence of anything local -- it is "how long since the last
// block", a number all six nodes compute identically.
func TestStuckChainDoesNotTriggerAResetRequestPerTick(t *testing.T) {
	var e Engine

	// Joins the chain at vo=300, i.e. thirty minutes after the frozen parent.
	if e.shouldRequestResetView(300) {
		t.Fatal("a node that joined a stuck chain asked for a reset on its first tick; " +
			"a large offset is not a fault signal")
	}
	// Every tick after that, up to one whole window short of maxViewOffset more.
	for vo := uint32(301); vo < 300+maxViewOffset; vo++ {
		if e.shouldRequestResetView(vo) {
			t.Fatalf("asked for a reset at view %d, after watching only %d views fail",
				vo, vo-300)
		}
	}
	// 100 whole views of watching rotation fail: this is the case the constant
	// exists for, so it must still fire.
	if !e.shouldRequestResetView(300 + maxViewOffset) {
		t.Fatalf("after watching %d whole views fail without a block, the node stayed silent",
			maxViewOffset)
	}
	// And not again immediately after.
	for vo := uint32(301 + maxViewOffset); vo < 300+2*maxViewOffset; vo++ {
		if e.shouldRequestResetView(vo) {
			t.Fatalf("asked twice inside one maxViewOffset window, at view %d", vo)
		}
	}
}

// TestStuckChainAsksOncePerWindowNotOncePerTick is the same fact stated as the
// count the live chain actually showed, because "not every tick" and "8900 against
// 8" are different claims and only the second one rules out a storm.
func TestStuckChainAsksOncePerWindowNotOncePerTick(t *testing.T) {
	var e Engine

	// Fifteen minutes of one-second ticks on a frozen anchor: vo walks from 249 to
	// 1148, which is the interval QA measured the 8900 requests in.
	fired := 0
	for vo := uint32(249); vo < 249+900; vo++ {
		if e.shouldRequestResetView(vo) {
			fired++
		}
	}
	if want := 8; fired != want {
		t.Fatalf("900 ticks of a stuck chain produced %d reset requests, want %d "+
			"(one per %d views); the storm was one per tick",
			fired, want, maxViewOffset)
	}
}

// TestLandedBlockReArmsTheResetGate covers the other direction: the rate limiter
// must not latch. A block lands, the grid is re-anchored, the offset drops to
// nearly zero, and the next stall has to be able to ask again -- otherwise the
// gate would be a permanent mute that no test would notice.
func TestLandedBlockReArmsTheResetGate(t *testing.T) {
	var e Engine

	// A first stall far enough along to have asked.
	for vo := uint32(0); vo <= maxViewOffset; vo++ {
		e.shouldRequestResetView(vo)
	}
	if !e.shouldRequestResetView(uint32(2 * maxViewOffset)) {
		t.Fatal("the first stall never asked for a reset")
	}

	// A block lands: the anchor moves, the derived offset drops.
	if e.shouldRequestResetView(2) {
		t.Fatal("a fresh view asked for a reset")
	}
	for vo := uint32(3); vo < 2+maxViewOffset; vo++ {
		if e.shouldRequestResetView(vo) {
			t.Fatalf("asked at view %d, only %d views after the block landed", vo, vo-2)
		}
	}
	if !e.shouldRequestResetView(2 + maxViewOffset) {
		t.Fatalf("the second stall never asked: the gate latched after the first one")
	}
}

// TestResetViewGateIsPerGeneration pins the re-basing itself, including the case
// the offset moving backwards covers: after a block lands the anchor is newer, so
// the next request is measured from the new grid and not from the old one.
func TestResetViewGateIsPerGeneration(t *testing.T) {
	var e Engine

	if e.shouldRequestResetView(50) {
		t.Fatal("asked before watching any views fail")
	}
	if e.shouldRequestResetView(150) != true {
		t.Fatal("did not ask after 100 views without a block")
	}
	// A block lands and the offset goes back to the start.
	if e.shouldRequestResetView(0) {
		t.Fatal("asked on the block that just landed")
	}
	if e.shouldRequestResetView(99) {
		t.Fatal("asked 99 views after the block landed")
	}
	if !e.shouldRequestResetView(100) {
		t.Fatal("did not ask 100 views after the block landed")
	}
}
