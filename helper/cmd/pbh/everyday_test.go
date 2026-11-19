package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
)

func TestSplitComponentFindsRelayOrEdgeAmongFlags(t *testing.T) {
	component, rest, err := splitComponent([]string{"--follow", "edge", "--since", "10m"})
	if err != nil || component != "edge" || strings.Join(rest, " ") != "--follow --since 10m" {
		t.Fatalf("got %q %q %v", component, rest, err)
	}
	if component, rest, err = splitComponent(nil); err != nil || component != "" || len(rest) != 0 {
		t.Fatalf("no component: %q %q %v", component, rest, err)
	}
	if _, _, err = splitComponent([]string{"relay", "edge"}); err == nil {
		t.Fatal("two components were accepted")
	}
}

// Bad arguments are refused before sudo, Docker or the installation are touched.
func TestEverydayCommandsRefuseBadArgumentsBeforeAnyChange(t *testing.T) {
	for _, args := range [][]string{
		{"logs", "--lines", "0"},
		{"logs", "--lines", "10001"},
		{"logs", "--since", "-5m"},
		{"logs", "relay", "edge"},
		{"logs", "both"},
		{"logs", "--tail", "5"},
		{"restart", "both"},
		{"restart", "relay", "edge"},
		{"update", "now"},
		{"update", "--force"},
	} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out); err == nil {
			t.Fatalf("pbh %v was accepted", args)
		}
	}
}

func TestPrefixWriterMarksEachLine(t *testing.T) {
	var out bytes.Buffer
	w := &prefixWriter{out: &out, prefix: "edge | ", lock: new(sync.Mutex)}
	w.Write([]byte("one\ntw"))
	w.Write([]byte("o\nthree"))
	w.flush()
	if out.String() != "edge | one\nedge | two\nedge | three\n" {
		t.Fatalf("got %q", out.String())
	}
}
