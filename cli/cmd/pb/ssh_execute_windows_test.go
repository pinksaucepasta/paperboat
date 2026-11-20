//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/command"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

func TestWindowsSSHRemoteCommandMatchesOpenSSHJoinSemantics(t *testing.T) {
	values := []string{"/usr/bin/sh", "-c", "'printf %s VICTUS_PB_SSH_OK'"}
	got, err := windowsSSHRemoteCommand(values)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join(values, " "); got != want {
		t.Fatalf("command=%q want=%q", got, want)
	}
	for _, invalid := range [][]string{nil, {"bad\x00argument"}} {
		if _, err := windowsSSHRemoteCommand(invalid); err == nil {
			t.Fatalf("invalid command %q accepted", invalid)
		}
	}
}

func TestWindowsSSHCommandInputLeavesTerminalUnowned(t *testing.T) {
	ctx := withWindowsSSHDependencies(t.Context(), func(d *windowsSSHDependencies) { d.isInputTerminal = func(int) bool { return true } })
	file, err := os.CreateTemp(t.TempDir(), "console")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	input, err := windowsSSHCommandInput(ctx, file)
	if err != nil || input != nil {
		t.Fatalf("terminal input=%v error=%v", input, err)
	}
}

func TestWindowsSSHCommandInputDuplicateCloseCancelsReadAndPreservesOriginal(t *testing.T) {
	ctx := withWindowsSSHDependencies(t.Context(), func(d *windowsSSHDependencies) { d.isInputTerminal = func(int) bool { return false } })
	original, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	defer writer.Close()
	input, err := windowsSSHCommandInput(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		close(started)
		_, readErr := input.Read(make([]byte, 1))
		readDone <- readErr
	}()
	<-started
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("closing owned input did not cancel its blocked read")
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	value := make([]byte, 1)
	if _, err := original.Read(value); err != nil || string(value) != "x" {
		t.Fatalf("original input value=%q error=%v", value, err)
	}
}

func withWindowsSSHDependencies(ctx context.Context, configure func(*windowsSSHDependencies)) context.Context {
	return withDependencies(ctx, defaultWindowsSSHDependencies, configure)
}

func TestWindowsManagedSSHDependenciesRequiresLocalDaemonBeforeDial(t *testing.T) {
	want := errors.New("daemon unavailable")
	cfg := &config.Config{}
	called := false
	set := flag.NewFlagSet("ssh", flag.ContinueOnError)
	ctx := command.NewContext(set, strings.NewReader(""))
	ctx.Context = withWindowsSSHDependencies(context.Background(), func(d *windowsSSHDependencies) {
		d.buildDeps = func(*command.Context) (*deps, error) {
			return &deps{cfg: cfg}, nil
		}
		d.requireDaemon = func(ctx context.Context, got *config.Config) error {
			called = true
			if ctx == nil || got != cfg {
				t.Fatalf("require daemon context=%v config=%p want=%p", ctx, got, cfg)
			}
			return want
		}
	})
	if _, err := windowsManagedSSHDependencies(ctx); !errors.Is(err, want) {
		t.Fatalf("dependencies error=%v", err)
	}
	if !called {
		t.Fatal("local daemon readiness was not required")
	}
}
