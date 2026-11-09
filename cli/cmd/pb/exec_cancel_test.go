package main

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/tunnel"
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
	var cancels, abandons atomic.Int32
	abandoned := make(chan struct{}, 2)
	go watchExecSignals(signals, callerDone, execDone, func() { cancels.Add(1) },
		func() { abandons.Add(1); abandoned <- struct{}{} }, func(os.Signal) {})
	defer close(execDone)
	close(callerDone) // the first Ctrl-C cancels the invocation's context ...
	signals <- os.Interrupt
	time.Sleep(50 * time.Millisecond) // ... and arrives on the signal channel too
	if abandons.Load() != 0 || cancels.Load() == 0 {
		t.Fatalf("after one Ctrl-C: cancels=%d abandons=%d", cancels.Load(), abandons.Load())
	}
	signals <- syscall.SIGTERM
	select {
	case <-abandoned:
	case <-time.After(5 * time.Second):
		t.Fatal("a second signal did not abandon the remote cancel")
	}
}
