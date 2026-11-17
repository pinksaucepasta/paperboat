package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntimecmd"
)

func TestPairWithoutSystemdChangesNothingAndPrintsTheForegroundCommand(t *testing.T) {
	old := hostruntimecmd.NoSystemd
	hostruntimecmd.NoSystemd = func() bool { return true }
	t.Cleanup(func() { hostruntimecmd.NoSystemd = old })
	oldBootstrap := pairBootstrap
	pairBootstrap = func(context.Context, []string, io.Reader, io.Writer, io.Writer) int {
		t.Fatal("pair reached the bootstrap on a machine without systemd")
		return 1
	}
	t.Cleanup(func() { pairBootstrap = oldBootstrap })
	command := pairCommand()
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--enrollment-token", "SECRETTOKEN123"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "daemon --foreground") || !strings.Contains(stderr.String(), "no systemd") || strings.Contains(stderr.String(), "SECRETTOKEN123") {
		t.Fatalf("message = %q", stderr.String())
	}
}
