//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

type terminalDisplayPTY struct {
	process     *pty.Process
	writes      int
	resizeCalls int
}

func (c *terminalDisplayPTY) Read(b []byte) (int, error)  { return c.process.Read(b) }
func (c *terminalDisplayPTY) Write(b []byte) (int, error) { c.writes++; return c.process.Write(b) }
func (c *terminalDisplayPTY) Close() error                { return c.process.CloseIO() }
func (c *terminalDisplayPTY) Wait() (int, error) {
	r, e := c.process.Wait(context.Background())
	return r.Code, e
}
func (c *terminalDisplayPTY) Resize(rows, cols uint16) error {
	c.resizeCalls++
	return c.process.Resize(pty.Dimensions{Rows: rows, Columns: cols})
}

// The shell has already displayed its prompt before the live attachment. Same
// geometry cannot generate WINCH; initialization must redraw without input.
func TestInitialTerminalDisplayRedrawsIdlePTYWithoutInput(t *testing.T) {
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	process, err := adapter.Start(pty.Command{Path: "/bin/sh", Args: []string{"-c", `trap 'printf "DISPLAY_REFRESHED:"; stty size' WINCH; printf 'DISPLAY_BEFORE_ATTACH\n'; while :; do sleep 10 & wait $!; done`}, Env: []string{"PATH=/usr/bin:/bin"}, CWD: root, Dimensions: pty.Dimensions{Rows: 24, Columns: 80}})
	if err != nil {
		t.Fatal(err)
	}
	conn := &terminalDisplayPTY{process: process}
	output := make(chan []byte, 16)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			b := make([]byte, 256)
			n, e := process.Read(b)
			if n > 0 {
				output <- b[:n]
			}
			if e != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		process.Terminate(ctx, 0)
		process.CloseIO()
		<-readerDone
	})
	waitMarker := func(marker string) {
		t.Helper()
		var seen []byte
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case b := <-output:
				seen = append(seen, b...)
				if bytes.Contains(seen, []byte(marker)) {
					return
				}
			case <-deadline.C:
				t.Fatal("idle terminal did not render the owned display marker")
			}
		}
	}
	waitMarker("DISPLAY_BEFORE_ATTACH")
	if err := initializeTerminalDisplay(conn, &resolver.TerminalTarget{}, 24, 80); err != nil {
		t.Fatal(err)
	}
	waitMarker("DISPLAY_REFRESHED:24 80")
	if conn.writes != 0 {
		t.Fatal("display initialization injected terminal input")
	}
}

func TestInitialTerminalDisplayPreservesSharedPermissionsAndRestoreErrors(t *testing.T) {
	for _, scope := range []string{"terminal:view", "terminal:control"} {
		conn := &terminalDisplayPTY{}
		target := &resolver.TerminalTarget{Auth: resolver.AuthTarget{Scopes: []string{scope}}}
		if err := initializeTerminalDisplay(conn, target, 24, 80); err != nil || conn.resizeCalls != 0 || conn.writes != 0 {
			t.Fatal("shared initial display mutated owner terminal", err)
		}
	}
	failure := io.ErrClosedPipe
	// A failed probe may have reached the remote PTY; original geometry must
	// still be attempted, preserving the transport failure as the cause.
	closed := &terminalDisplayFailureConn{err: failure}
	if err := initializeTerminalDisplay(closed, &resolver.TerminalTarget{}, 24, 80); !errors.Is(err, failure) || closed.lastRows != 24 || closed.lastCols != 80 || closed.calls != 2 {
		t.Fatal("redraw failure lost requested geometry or cause", err)
	}
}

type terminalDisplayFailureConn struct {
	err                error
	calls              int
	lastRows, lastCols uint16
}

func (c *terminalDisplayFailureConn) Read([]byte) (int, error)  { return 0, c.err }
func (c *terminalDisplayFailureConn) Write([]byte) (int, error) { return 0, c.err }
func (c *terminalDisplayFailureConn) Close() error              { return nil }
func (c *terminalDisplayFailureConn) Wait() (int, error)        { return 0, c.err }
func (c *terminalDisplayFailureConn) Resize(r, col uint16) error {
	c.calls++
	c.lastRows, c.lastCols = r, col
	return c.err
}
