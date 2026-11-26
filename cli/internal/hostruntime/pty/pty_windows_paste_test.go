//go:build windows

package pty

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pb no longer turns bracketed paste on in the local terminal, so a Windows shell or program that asks for it must be
// heard through ConPTY: its DECSET 2004 has to reach the host's output.
func TestConPTYPassesBracketedPasteModeThrough(t *testing.T) {
	root := t.TempDir()
	adapter, err := NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	process, err := adapter.Start(Command{
		Path: filepath.Join(os.Getenv("WINDIR"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
		Args: []string{"-NoLogo", "-NoProfile", "-Command", "[Console]::Write([string][char]27 + '[?2004h'); Start-Sleep 1; [Console]::Write([string][char]27 + '[?2004l'); Start-Sleep 30"},
		CWD:  root, Dimensions: Dimensions{Columns: 80, Rows: 25},
	})
	if err != nil {
		t.Fatal(err)
	}
	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		buffer := make([]byte, 4096)
		for {
			n, err := process.Read(buffer)
			if n > 0 {
				chunks <- bytes.Clone(buffer[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		go func() { // closing a pseudoconsole waits for its output to be read
			for range chunks {
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := process.Terminate(ctx, 0); err != nil {
			t.Errorf("terminate: %v", err)
		}
		_ = process.CloseIO()
	}()
	var output []byte
	deadline := time.After(25 * time.Second)
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatalf("ConPTY output ended: %q", output)
			}
			output = append(output, chunk...)
			if on := bytes.Index(output, []byte("\x1b[?2004h")); on >= 0 && bytes.Contains(output[on:], []byte("\x1b[?2004l")) {
				return
			}
		case <-deadline:
			t.Fatalf("no bracketed paste on and off in ConPTY output: %q", output)
		}
	}
}
