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

package ethconfig

// Guards for the one secret this client carries: pbft.Config.KeyStorePassword.
//
// The whole protection is a single struct tag (toml:"-"). Remove it and
// everything still compiles, every other test still passes, and the keystore
// password starts appearing in `geth dumpconfig` output and in any config file
// the node writes -- a silent, total loss of the secret. So the tag is pinned
// two ways: by reading it back off the type (intent) and by serializing a
// sentinel through the same encoder `dumpconfig` uses (mechanism). Either one
// alone is defeatable; the pair is not, because a change that satisfies one and
// breaks the other fails the suite.
//
// It is the same defence p2p.Config.PrivateKey already uses upstream
// (p2p/config.go:37, `toml:"-"`).
//
// If a future field carries a secret, it belongs in the table below.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/naoina/toml"

	"github.com/ethereum/go-ethereum/consensus/pbft"
)

// secretSentinel is deliberately impossible to confuse with a real value: a test
// that fails prints it, and a test that passes never sees it.
const secretSentinel = "SENTINEL-do-not-serialize-2c1f9e"

// dumpConfigTOML mirrors the encoder settings cmd/geth uses for `dumpconfig`
// (cmd/geth/config.go:73-95): field names are used verbatim as TOML keys.
//
// It is duplicated rather than imported because gethConfig, the struct
// dumpconfig actually marshals, lives in package main. The part that decides
// whether a secret leaks is the nested pbft.Config's own tags, and that is what
// these tests exercise.
var dumpConfigTOML = toml.Config{
	NormFieldName: func(rt reflect.Type, key string) string { return key },
	FieldToKey:    func(rt reflect.Type, field string) string { return field },
}

// TestKeyStorePasswordIsExcludedFromTOML pins the intent: the field exists and it
// is tagged out of TOML.
//
// A rename would otherwise make the serialization test below pass vacuously --
// FieldByName would return ok=false and there would be nothing to leak.
func TestKeyStorePasswordIsExcludedFromTOML(t *testing.T) {
	field, ok := reflect.TypeOf(pbft.Config{}).FieldByName("KeyStorePassword")
	if !ok {
		t.Fatal("pbft.Config has no KeyStorePassword field: if it was renamed, move this guard with it")
	}
	if got := field.Tag.Get("toml"); got != "-" {
		t.Fatalf("pbft.Config.KeyStorePassword toml tag is %q, want \"-\": without it the password "+
			"is written to disk by `geth dumpconfig` and by any config file the node rewrites", got)
	}
}

// TestDumpConfigDoesNotEmitKeyStorePassword pins the mechanism: a populated field
// must not reach the serialized output.
//
// It complements the tag assertion rather than repeating it. A future refactor
// could keep the tag and still leak -- e.g. by adding a MarshalTOML method to
// pbft.Config that writes the field under another name -- and only this test
// would notice.
func TestDumpConfigDoesNotEmitKeyStorePassword(t *testing.T) {
	cfg := Config{
		Pbft: pbft.Config{
			Enable:           true,
			KeyStore:         "/tmp/arbiter-0.dat",
			KeyStorePassword: secretSentinel,
			MinerAddress:     "0x8497B607F22941e770C436B05E89440195Eb0E60",
		},
	}
	out, err := dumpConfigTOML.Marshal(&cfg)
	if err != nil {
		t.Fatalf("cannot marshal the eth config: %v", err)
	}
	text := string(out)

	// The section has to be there at all: a guard that only greps for the secret
	// would pass just as happily if [Pbft] vanished, and then it guards nothing.
	if !strings.Contains(text, "[Pbft]") {
		t.Fatalf("the serialized config has no [Pbft] section, so this guard is not testing anything:\n%s", text)
	}
	if strings.Contains(text, secretSentinel) {
		t.Fatalf("the keystore password was serialized into the config:\n%s", text)
	}
	// The key name, in both the Go spelling and the snake_case one, so a rename
	// of the field does not sneak the value past the sentinel check above.
	lower := strings.ToLower(text)
	for _, key := range []string{"keystorepassword", "key_store_password"} {
		if strings.Contains(lower, key) {
			t.Fatalf("the serialized config still carries a %q key:\n%s", key, text)
		}
	}
}

// TestTOMLConfigCannotSupplyKeyStorePassword closes the inbound direction:
// whatever `dumpconfig` writes, `--config` reads. If a password could be loaded
// from a config file, the outbound guard would be pointless -- the file is the
// leak.
//
// Either outcome is acceptable (an error from the decoder, or a silently ignored
// key); what is not acceptable is the value arriving in the field. Today the
// decoder errors, and the message is asserted because it names the tag as the
// reason -- which is exactly the invariant being protected.
func TestTOMLConfigCannotSupplyKeyStorePassword(t *testing.T) {
	const table = "[Pbft]\nEnable = true\nKeyStorePassword = \"" + secretSentinel + "\"\n"

	var cfg Config
	// dumpConfigTOML, not a zero toml.Config: the decoder calls
	// Config.NormFieldName unconditionally and panics when it is nil
	// (naoina/toml util.go:34), which is why cmd/geth always sets it.
	err := dumpConfigTOML.NewDecoder(strings.NewReader(table)).Decode(&cfg)
	if cfg.Pbft.KeyStorePassword != "" {
		t.Fatalf("a config file supplied the keystore password (%q), so TOML is a password channel again "+
			"(decoder error: %v)", cfg.Pbft.KeyStorePassword, err)
	}
	if err == nil {
		// Silent acceptance is tolerable; a reporter that says nothing is not.
		t.Log("the decoder ignored KeyStorePassword instead of rejecting it")
		return
	}
	if !strings.Contains(err.Error(), "cannot be set through TOML") {
		t.Fatalf("the decoder rejected the key for the wrong reason: %v", err)
	}
}
