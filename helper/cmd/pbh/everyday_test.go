package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/reporting"
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

// On a host with no claimed installation the commands say what to do, not "operation failed".
func TestEverydayCommandsGuideAnUnclaimedOrMissingInstallation(t *testing.T) {
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "false")
	reporter, err := reporting.New("paperboat-selfhost")
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	missing := filepath.Join(t.TempDir(), "PRIVATE_INSTALLATION")
	for command, want := range map[string]string{"logs": "self-host installer", "restart": "self-host installer", "update": "not claimed yet"} {
		args := []string{command, "--state-dir", missing}
		if command == "restart" {
			args = append(args, "relay")
		}
		var public bytes.Buffer
		if code := execute(context.Background(), reporter, args, io.Discard, &public); code != 1 || !strings.Contains(public.String(), want) || strings.Contains(public.String(), "PRIVATE_INSTALLATION") {
			t.Fatalf("pbh %v: code %d, output %q", args, code, public.String())
		}
	}
}
