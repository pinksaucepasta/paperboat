//go:build darwin || linux

package pty

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShellAdapterStartsInOutsideDirectoryAndRejectsInvalidDirectory(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	adapter, err := NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	command := Command{Path: shellPath(t), Args: []string{"-c", "pwd -P"}, Env: []string{"PATH=/usr/bin:/bin"}, CWD: outside, Dimensions: Dimensions{80, 24}}
	process, err := adapter.Start(command)
	if err != nil {
		t.Fatal(err)
	}
	defer process.CloseIO()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := io.ReadAll(process)
	if err != nil {
		t.Fatal(err)
	}
	result, err := process.Wait(ctx)
	want, _ := filepath.EvalSymlinks(outside)
	if err != nil || result.Code != 0 || strings.TrimSpace(string(output)) != want {
		t.Fatalf("pwd=%q exit=%d err=%v", output, result.Code, err)
	}
	file := filepath.Join(outside, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{file, filepath.Join(outside, "missing"), "relative"} {
		command.CWD = cwd
		if _, err := adapter.Start(command); !errors.Is(err, ErrInvalidCWD) {
			t.Fatalf("cwd=%q err=%v", cwd, err)
		}
	}
}
