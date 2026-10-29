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

// The ported dpos call sites pass bare identifiers as log arguments rather than
// formatted strings -- Info("is self", self), Info("acceptVotes count:", n) --
// because that is how the upstream logger was called. Their readability depends
// entirely on the separator the helper inserts, which is invisible to the
// compiler and to every test that only checks behaviour. It broke once already:
// the helpers used fmt.Sprint, which adds a space only when neither operand is a
// string, so "is self" + true printed fine while "viewOffset" + "3" printed
// "viewOffset3" -- roughly 300 call sites, silently, at every verbosity.

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"

	glog "github.com/ethereum/go-ethereum/log"
)

// recordingHandler captures the message a logger is handed, so a test can assert
// what the helper emits rather than what a formatter next to it would emit.
type recordingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) captured() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.msgs...)
}

// TestLogHelpersKeepSprintlnSpacing pins one property: the dpos log helpers
// separate every operand, the way fmt.Sprintln does, and strip exactly the one
// newline Sprintln appends.
//
// The name says which semantics it guards rather than naming a file or a
// function, because the failure mode here is a formatter swap that leaves all
// behaviour intact. Two subtests, because they fail for different reasons: the
// formatting rule can be right while the four helpers no longer call it.
func TestLogHelpersKeepSprintlnSpacing(t *testing.T) {
	t.Run("formatting", func(t *testing.T) {
		cases := []struct {
			name string
			args []interface{}
			want string
		}{
			{
				// The regression this file exists for: fmt.Sprint fuses two
				// string operands into "is selftrue".
				name: "two string operands stay two words",
				args: []interface{}{"is self", "true"},
				want: "is self true",
			},
			{
				name: "a trailing string operand is not glued to what precedes it",
				args: []interface{}{"viewOffset", "3"},
				want: "viewOffset 3",
			},
			{
				// fmt.Sprintln() alone is a single newline; the result must be
				// empty, not a newline the caller never asked for.
				name: "no operands renders nothing",
				args: nil,
				want: "",
			},
			{
				name: "one string operand is passed through",
				args: []interface{}{"hello"},
				want: "hello",
			},
			{
				name: "a single error renders its message",
				args: []interface{}{errors.New("boom")},
				want: "boom",
			},
			{
				name: "string and int are separated",
				args: []interface{}{"count:", 3},
				want: "count: 3",
			},
			{
				// Sprintln("a\n") is "a\n\n"; dropping one newline leaves the
				// caller's own, which is the contract being pinned here.
				name: "only sprintln's own newline is removed",
				args: []interface{}{"a\n"},
				want: "a\n",
			},
			{
				name: "a caller's newlines are preserved",
				args: []interface{}{"a\n\n"},
				want: "a\n\n",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := sprintArgs(tc.args...); got != tc.want {
					t.Fatalf("sprintArgs(%#v) = %q, want %q", tc.args, got, tc.want)
				}
			})
		}
	})

	// The formatting subtest above would still pass if the four helpers were
	// changed back to fmt.Sprint, so this asserts the wiring end to end: swap the
	// root logger for a recorder and check what each level actually emits.
	t.Run("all four helpers route through it", func(t *testing.T) {
		previous := glog.Root()
		recorder := &recordingHandler{}
		glog.SetDefault(glog.NewLogger(recorder))
		defer glog.SetDefault(previous)

		Debug("debug", "x")
		Info("info", "x")
		Warn("warn", "x")
		Error("error", "x")

		want := []string{"debug x", "info x", "warn x", "error x"}
		if got := recorder.captured(); !slices.Equal(got, want) {
			t.Fatalf("helpers emitted %#v, want %#v", got, want)
		}
	})
}
