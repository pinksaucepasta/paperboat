package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/tunnel"
)

// A dial that opened just as the user pressed Ctrl-C was closed and reported as "did not run", though the host may
// have started the command. The open connection is kept so the cancel path stops the command remotely.
func TestExecDialThatOpensAsTheCallerCancelsKeepsTheConnection(t *testing.T) {
	caller, cancel := context.WithCancel(context.Background())
	opened := &cutConn{host: &hostStdin{}}
	connection, err := dialExecAttached(caller, caller, func(context.Context) (tunnel.ExecConn, error) {
		cancel() // Ctrl-C lands while the host accepts the start
		return opened, nil
	})
	if err != nil || connection != opened {
		t.Fatalf("dial = %v, %v; want the opened connection", connection, err)
	}
}

// A second Ctrl-C (or SIGTERM) abandons the bounded remote cancel, so the CLI exits now instead of after 25 s. The
// first Ctrl-C reaches both the caller's context and the signal channel, and counts once.
func TestExecSecondInterruptAbandonsTheRemoteCancel(t *testing.T) {
	signals := make(chan os.Signal, 1)
	callerDone, execDone := make(chan struct{}), make(chan struct{})
	cancelled, abandoned := make(chan struct{}, 4), make(chan struct{}, 2)
	go watchExecSignals(signals, callerDone, execDone, func() { cancelled <- struct{}{} },
		func() { abandoned <- struct{}{} }, func(os.Signal) {})
	defer close(execDone)
	close(callerDone)       // the first Ctrl-C cancels the invocation's context ...
	signals <- os.Interrupt // ... and arrives on the signal channel too
	// The watcher handles events one at a time: once both cancels are seen, the first Ctrl-C has been counted.
	for range 2 {
		select {
		case <-cancelled:
		case <-abandoned:
			t.Fatal("one Ctrl-C abandoned the remote cancel")
		case <-time.After(30 * time.Second):
			t.Fatal("the first Ctrl-C did not cancel")
		}
	}
	signals <- syscall.SIGTERM
	select {
	case <-abandoned:
	case <-time.After(30 * time.Second):
		t.Fatal("a second signal did not abandon the remote cancel")
	}
}
