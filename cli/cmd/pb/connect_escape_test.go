package main

import (
	"bytes"
	"sync"
	"testing"
)

type escapeSink struct {
	mu  sync.Mutex
	got bytes.Buffer
}

func (s *escapeSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got.Write(p)
}
func (s *escapeSink) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.got.String() }

func runEscape(t *testing.T, writes ...string) (sent string, reconnects, disconnects int) {
	t.Helper()
	sink := &escapeSink{}
	in := &escapeInput{out: sink, lineStart: true, reconnect: func() { reconnects++ }, disconnect: func() { disconnects++ }}
	for _, w := range writes {
		if n, err := in.Write([]byte(w)); err != nil || n != len(w) {
			t.Fatalf("write %q: n=%d err=%v", w, n, err)
		}
	}
	return sink.String(), reconnects, disconnects
}

func TestEscapeInput(t *testing.T) {
	for _, tc := range []struct {
		name        string
		writes      []string
		sent        string
		reconnects  int
		disconnects int
	}{
		{"Enter ~ r reconnects", []string{"ls", "\r", "~", "r"}, "ls\r", 1, 0},
		{"~ r at the very start reconnects", []string{"~", "r"}, "", 1, 0},
		{"Enter ~ . disconnects", []string{"\r", "~", "."}, "\r", 0, 1},
		{"~~ sends one ~", []string{"\r", "~", "~"}, "\r~", 0, 0},
		{"~ then another key sends both", []string{"\r", "~", "x"}, "\r~x", 0, 0},
		{"~ mid-line is plain", []string{"a", "~", "r"}, "a~r", 0, 0},
		{"a paste containing a newline then ~r is plain", []string{"echo a\r~r\r"}, "echo a\r~r\r", 0, 0},
		{"a paste chunk starting with ~ is plain", []string{"\r", "~r more"}, "\r~r more", 0, 0},
		{"a bracketed paste split at a newline is plain", []string{"\x1b[200~a\r", "~", "r\x1b[201~"}, "\x1b[200~a\r~r\x1b[201~", 0, 0},
		{"after a paste ends escapes work again", []string{"\x1b[200~x\x1b[201~", "\r", "~", "r"}, "\x1b[200~x\x1b[201~\r", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent, r, d := runEscape(t, tc.writes...)
			if sent != tc.sent || r != tc.reconnects || d != tc.disconnects {
				t.Fatalf("sent=%q reconnects=%d disconnects=%d, want %q %d %d", sent, r, d, tc.sent, tc.reconnects, tc.disconnects)
			}
		})
	}
}
