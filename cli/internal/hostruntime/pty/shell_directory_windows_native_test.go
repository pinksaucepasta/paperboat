//go:build windows && paperboat_native_e2e

package pty

import (
	"context"
	"errors"
	"github.com/charmbracelet/x/ansi"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeShellAdapterCurrentDirectoryOutsideRoot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	adapter, err := NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	command := Command{Path: filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), Args: []string{"/D", "/Q", "/C", "cd"}, CWD: outside, Dimensions: Dimensions{80, 25}}
	process, err := adapter.Start(command)
	if err != nil {
		t.Fatal(err)
	}
	defer process.CloseIO()
	output := collectNativeOutput(process)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := process.Wait(ctx)
	if err != nil || result.Code != 0 {
		t.Fatalf("exit=%+v err=%v", result, err)
	}
	select {
	case <-output.done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !strings.EqualFold(strings.TrimSpace(ansi.Strip(output.String())), outside) {
		t.Fatalf("cwd=%q want=%q", output.String(), outside)
	}
	scoped, err := NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.Start(command); !errors.Is(err, ErrInvalidCWD) {
		t.Fatalf("scoped adapter escape=%v", err)
	}
	command.CWD = filepath.Join(outside, "missing")
	if _, err := adapter.Start(command); !errors.Is(err, ErrInvalidCWD) {
		t.Fatalf("missing directory=%v", err)
	}
}
