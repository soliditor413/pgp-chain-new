// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package pbft

import (
	"testing"

	dposmsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"
)

// TestSetWireVersionV2 pins the O1 decision: the DPoS payload version is set
// unconditionally, not gated on a height.
//
// If this test ever fails because the value stayed below DPoSV2Version, every
// version/ping/pong this node sends uses the V1 field set
// (dpos/p2p/msg/version.go:60/67/74/97) while a pgp-chain 1.9.7 peer expects V2.
// Neither side logs an error -- each decodes a payload of the wrong length, fails
// the checksum and drops the connection.
func TestSetWireVersionV2(t *testing.T) {
	SetWireVersionV2()

	if got := WireVersion(); got != DPoSWireVersionV2 {
		t.Fatalf("WireVersion() = %d after SetWireVersionV2, want %d", got, DPoSWireVersionV2)
	}
}

// TestWireVersionV2ConstantMatchesElastos guards the re-export: if the vendored
// constant changes, this fails rather than silently pinning a stale number.
func TestWireVersionV2ConstantMatchesElastos(t *testing.T) {
	if DPoSWireVersionV2 != dposmsg.DPoSV2Version {
		t.Fatalf("DPoSWireVersionV2 = %d, Elastos DPoSV2Version = %d",
			DPoSWireVersionV2, dposmsg.DPoSV2Version)
	}
	if DPoSWireVersionV2 == 0 {
		t.Fatal("DPoSWireVersionV2 is 0; V2 must be distinguishable from the default")
	}
}
