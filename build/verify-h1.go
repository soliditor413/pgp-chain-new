// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

//go:build ignore
// +build ignore

// verify-h1 prints the go.sum-style h1 hash of a Go module zip, so the h1
// recorded for third_party/ can be recomputed from the archived zip instead of
// being taken on faith. It deliberately does not carry an expected value: the
// comparison belongs to the caller, otherwise the h1 would be hardcoded a
// second time and become another fact that can drift.
//
// Usage: go run build/verify-h1.go <module-zip>
//
// The tag is "ignore", not the "none" used by build/ci.go and
// build/update-license.go. Both exclude the file from ordinary builds, but
// `go mod tidy` treats "none" as a tag that could be set and therefore counts
// this file's imports, which would promote golang.org/x/mod from indirect to a
// direct requirement. "ignore" is skipped by the go command, so go.mod stays
// untouched. See `go mod tidy -diff` (expected: empty).
//
// It depends on golang.org/x/mod, which this module requires only indirectly;
// if a future tidy drops that indirect edge, this script stops compiling.
package main

import (
	"fmt"
	"os"

	"golang.org/x/mod/sumdb/dirhash"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run build/verify-h1.go <module-zip>")
		os.Exit(2)
	}
	h, err := dirhash.HashZip(os.Args[1], dirhash.Hash1)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-h1:", err)
		os.Exit(1)
	}
	fmt.Println(h)
}
