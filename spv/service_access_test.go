package spv

import "testing"

// TestNotReadyAccessors is the regression test for the pgp-chain panic:
// every precompile dereferenced the package level SpvService directly, so a
// node whose SPV had not started crashed mid-block instead of failing the block.
func TestNotReadyAccessors(t *testing.T) {
	saved := SpvService
	SpvService = nil
	defer func() { SpvService = saved }()

	if ready() {
		t.Fatal("ready() = true with a nil SpvService")
	}
	if _, err := GetELAHeader(1); err != errSPVNotReady {
		t.Fatalf("GetELAHeader error = %v, want errSPVNotReady", err)
	}
	if _, err := GetHeaderBest(); err != errSPVNotReady {
		t.Fatalf("GetHeaderBest error = %v, want errSPVNotReady", err)
	}

	// An allocated Service whose embedded SPVService is still nil must not panic
	// either: HeaderStore() would dereference the nil interface.
	SpvService = &Service{}
	if ready() {
		t.Fatal("ready() = true with a nil embedded SPVService")
	}
	if _, err := GetELAHeader(1); err != errSPVNotReady {
		t.Fatalf("GetELAHeader error = %v, want errSPVNotReady", err)
	}
	if _, err := GetHeaderBest(); err != errSPVNotReady {
		t.Fatalf("GetHeaderBest error = %v, want errSPVNotReady", err)
	}
}
