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
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// TestReadPbftPasswordFile pins the trailing-newline rules of the password file.
//
// The CRLF row is the one that matters and it is not hypothetical: pgp-chain's
// MakeDposPasswordList (flags.go:1217-1229) trims "\r" then "\n" as two separate
// passes, which leaves "pw\r" for a CRLF file -- the "\r" was stripped first,
// so the second pass finds no "\n" at the end and the carriage return survives
// into the password. The keystore then answers "password wrong", which reads as
// a wrong password rather than a wrong file. This port trims both characters in
// one pass, and this row is what keeps that from regressing.
//
// The multi-line row guards the other half: a file with two lines is a mistake
// in how it was written, and guessing which line was meant would turn it into
// the same undiagnosable "password wrong".
//
// The BOM and NUL rows are the same class of bug reached from Windows: an editor
// that prepends a BOM, or a shell that writes UTF-16LE. Both used to be returned
// verbatim -- "\ufeffpw", or "p\x00w\x00" -- so the operator saw "password
// wrong" for a password that looks correct in any editor. The BOM is stripped;
// UTF-16 is refused because decoding it needs the byte order and a wrong guess is
// just another wrong password.
//
// The three empty rows (0 bytes, one "\n", one "\r\n") are errors. They used to
// return "", on the theory that the keystore rejects an empty password anyway,
// but nothing in this tree can hold one -- keygen refuses to create it -- so an
// empty file is always a mistake and naming it beats "password wrong".
func TestReadPbftPasswordFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(t *testing.T, name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	longPath := write(t, "multi", "pw\nextra\n")
	// What `echo pw > pw.txt` under Windows PowerShell actually writes: UTF-16LE
	// with a BOM, i.e. a NUL after every ASCII byte.
	utf16Path := write(t, "utf16", "\xff\xfep\x00w\x00\r\x00\n\x00")

	tests := []struct {
		name    string
		content string
		path    string
		want    string
		wantErr bool
	}{
		{name: "trailing LF", content: "pw\n", want: "pw"},
		{name: "trailing CRLF", content: "pw\r\n", want: "pw"},
		{name: "no newline", content: "pw", want: "pw"},
		{name: "inner spaces kept", content: " p w \n", want: " p w "},
		{name: "utf8 BOM stripped", content: "\ufeffpw\n", want: "pw"},
		{name: "empty file", content: "", wantErr: true},
		{name: "only LF", content: "\n", wantErr: true},
		{name: "only CRLF", content: "\r\n", wantErr: true},
		{name: "only BOM", content: "\ufeff", wantErr: true},
		{name: "NUL bytes", content: "p\x00w\x00", wantErr: true},
		{name: "utf16 with BOM", path: utf16Path, wantErr: true},
		{name: "two lines", path: longPath, wantErr: true},
		{name: "missing file", path: filepath.Join(dir, "does-not-exist"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := tt.path
			if path == "" {
				path = write(t, strings.ReplaceAll(tt.name, " ", "-"), tt.content)
			}
			got, err := readPbftPasswordFile(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("readPbftPasswordFile(%q) = %q, want error", path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("readPbftPasswordFile(%q): %v", path, err)
			}
			if got != tt.want {
				t.Errorf("readPbftPasswordFile(%q) = %q, want %q", path, got, tt.want)
			}
		})
	}
}

// TestSetPbftResolvesPasswordFile is the end-to-end shape of the flag: what the
// operator passes is a path, what lands in the config is the password.
//
// This is the assertion that would have caught the bug this change fixes. The
// flag name matches pgp-chain's and pgp-chain reads a file, but this tree used
// to assign the raw flag value straight into cfg.Pbft.KeyStorePassword. A node
// configured the pgp-chain way therefore tried to unlock its keystore with a
// filesystem path: GetDposAccount failed, the engine stayed a verifier, and
// nothing above Debug level said so.
//
// The second half asserts the empty case: no flag means no password, so the
// node does not silently pick up whatever happens to be in a file.
func TestSetPbftResolvesPasswordFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pbft-password")
	const password = "correct horse battery staple"
	if err := os.WriteFile(path, []byte(password+"\n"), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}

	run := func(t *testing.T, args []string) *ethconfig.Config {
		t.Helper()
		var seen *ethconfig.Config
		app := &cli.App{
			Flags: []cli.Flag{PbftKeyStorePasswordFlag},
			Action: func(ctx *cli.Context) error {
				cfg := &ethconfig.Config{}
				setPbft(ctx, cfg)
				seen = cfg
				return nil
			},
		}
		if err := app.Run(append([]string{"geth"}, args...)); err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		if seen == nil {
			t.Fatal("setPbft never ran")
		}
		return seen
	}

	t.Run("path is read", func(t *testing.T) {
		cfg := run(t, []string{"--" + PbftKeyStorePasswordFlag.Name, path})
		if cfg.Pbft.KeyStorePassword != password {
			t.Errorf("KeyStorePassword = %q, want %q (the file's contents, not its path)",
				cfg.Pbft.KeyStorePassword, password)
		}
	})

	t.Run("no flag means no password", func(t *testing.T) {
		cfg := run(t, nil)
		if cfg.Pbft.KeyStorePassword != "" {
			t.Errorf("KeyStorePassword = %q, want empty", cfg.Pbft.KeyStorePassword)
		}
	})
}
