// Copyright 2019 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

// Package utils contains internal helper functions for go-ethereum commands.
package utils

import (
	"flag"
	"reflect"
	"testing"

	"github.com/urfave/cli/v2"

	"github.com/ethereum/go-ethereum/consensus/pbft"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
)

func Test_SplitTagsFlag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args string
		want map[string]string
	}{
		{
			"2 tags case",
			"host=localhost,bzzkey=123",
			map[string]string{
				"host":   "localhost",
				"bzzkey": "123",
			},
		},
		{
			"1 tag case",
			"host=localhost123",
			map[string]string{
				"host": "localhost123",
			},
		},
		{
			"empty case",
			"",
			map[string]string{},
		},
		{
			"garbage",
			"smth=smthelse=123",
			map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SplitTagsFlag(tt.args); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitTagsFlag() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPbftNetFlagDefaultsMatchTheEngine pins the two DPoS network defaults to the
// constants the engine falls back to.
//
// The failure this prevents is not "the flag has the wrong number in it": it is
// that cmd/utils and eth each carry their own copy of the default, drift apart,
// and then a rig where one node was started with the flag and another without
// ends up with two different magics. That mismatch closes every DPoS connection
// at Debug level (dpos/p2p/hub/hub.go:320), so the symptom is a producer set
// that never connects while every log line looks healthy.
func TestPbftNetFlagDefaultsMatchTheEngine(t *testing.T) {
	if PbftNetMagicFlag.Value != uint64(pbft.DefaultNetMagic) {
		t.Errorf("--pbft.net.magic default = %d, engine default = %d",
			PbftNetMagicFlag.Value, pbft.DefaultNetMagic)
	}
	if uint32(PbftMaxNodePerHostFlag.Value) != pbft.DefaultMaxNodePerHost {
		t.Errorf("--pbft.net.maxnodeperhost default = %d, engine default = %d",
			PbftMaxNodePerHostFlag.Value, pbft.DefaultMaxNodePerHost)
	}
}

// TestSetPbftFillsZeroNetworkSettings covers the case of a node started with no
// --pbft.net.* flags and no config file at all, which is the shape both a devnet
// rig and a fresh operator machine have.
//
// Each zero here is a silent failure rather than a startup error, which is why
// the fallback exists and why it is worth a test:
//
//   - Port 0          -> kernel picks an ephemeral port, node advertises the one
//     it did not get
//   - Magic 0         -> every DPoS connection is mistaken for a sub-service
//   - MaxNodePerHost 0-> every inbound DPoS connection is refused, at Debug level
func TestSetPbftFillsZeroNetworkSettings(t *testing.T) {
	set := flag.NewFlagSet("pbft", flag.ContinueOnError)
	ctx := cli.NewContext(cli.NewApp(), set, nil)
	cfg := &ethconfig.Config{}

	setPbft(ctx, cfg)

	if cfg.Pbft.IPAddress != "127.0.0.1" {
		t.Errorf("IPAddress = %q, want 127.0.0.1", cfg.Pbft.IPAddress)
	}
	if cfg.Pbft.Port != 20679 {
		t.Errorf("Port = %d, want 20679", cfg.Pbft.Port)
	}
	if cfg.Pbft.Magic != pbft.DefaultNetMagic {
		t.Errorf("Magic = %d, want %d", cfg.Pbft.Magic, pbft.DefaultNetMagic)
	}
	if cfg.Pbft.MaxNodePerHost != pbft.DefaultMaxNodePerHost {
		t.Errorf("MaxNodePerHost = %d, want %d",
			cfg.Pbft.MaxNodePerHost, pbft.DefaultMaxNodePerHost)
	}
}

// TestSetPbftKeepsExplicitValues is the other half: the fallback must not
// overwrite what the operator actually asked for.
//
// The context has to come from a real App run rather than a bare flag.FlagSet,
// because cli/v2 registers its flags into the set on Run; parsing a set that was
// never populated would leave every IsSet false and the test would silently
// assert the fallback path twice.
func TestSetPbftKeepsExplicitValues(t *testing.T) {
	var seen *ethconfig.Config

	app := &cli.App{
		Flags: []cli.Flag{
			PbftNetAddressFlag, PbftNetPortFlag, PbftNetMagicFlag, PbftMaxNodePerHostFlag,
		},
		Action: func(ctx *cli.Context) error {
			cfg := &ethconfig.Config{}
			setPbft(ctx, cfg)
			seen = cfg
			return nil
		},
	}
	err := app.Run([]string{
		"geth",
		"--" + PbftNetAddressFlag.Name, "10.0.0.7",
		"--" + PbftNetPortFlag.Name, "20700",
		"--" + PbftNetMagicFlag.Name, "777",
		"--" + PbftMaxNodePerHostFlag.Name, "8",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if seen == nil {
		t.Fatal("setPbft never ran")
	}
	if seen.Pbft.IPAddress != "10.0.0.7" {
		t.Errorf("IPAddress = %q, want 10.0.0.7 (explicit flag overwritten)",
			seen.Pbft.IPAddress)
	}
	if seen.Pbft.Port != 20700 {
		t.Errorf("Port = %d, want 20700", seen.Pbft.Port)
	}
	if seen.Pbft.Magic != 777 {
		t.Errorf("Magic = %d, want 777", seen.Pbft.Magic)
	}
	if seen.Pbft.MaxNodePerHost != 8 {
		t.Errorf("MaxNodePerHost = %d, want 8", seen.Pbft.MaxNodePerHost)
	}
}
