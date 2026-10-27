//go:build !windows

package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
)

// N10/F3-06: keys typed while pb starts must reach the command. A package that asks the terminal for its colours
// at init (bubbletea v1 did) writes OSC 11 + DSR and discards stdin until the reply; a terminal that never answers
// (a PTY under sudo, a harness) then loses everything typed until the timeout. pb's whole import graph runs its
// init in this test binary, under a PTY that never answers: nothing may be queried and the typed bytes must still
// be unread.
func TestStartupNeverQueriesTheTerminal(t *testing.T) {
	if os.Getenv("PB_STARTUP_QUERY_CHILD") == "1" {
		// init has run; leave the typed bytes in the terminal and report what is still readable.
		time.Sleep(300 * time.Millisecond)
		buf := make([]byte, 64)
		n, _ := os.Stdin.Read(buf)
		_, _ = os.Stderr.WriteString("STDIN:" + string(buf[:n]) + "\n")
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestStartupNeverQueriesTheTerminal$", "-test.timeout=20s")
	child.Env = append(os.Environ(), "PB_STARTUP_QUERY_CHILD=1", "TERM=xterm-256color")
	terminal, err := pty.Start(child)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if _, err := terminal.Write([]byte("typed-ahead\n")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&output, terminal); close(done) }()
	if err := child.Wait(); err != nil {
		t.Fatalf("child: %v\n%q", err, output.String())
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	got := output.Bytes()
	if bytes.Contains(got, []byte("\x1b]11;?")) || bytes.Contains(got, []byte("\x1b[6n")) {
		t.Fatalf("pb queried the terminal at startup: %q", got)
	}
	if !bytes.Contains(got, []byte("STDIN:typed-ahead")) {
		t.Fatalf("keys typed during startup did not reach the command: %q", got)
	}
}
