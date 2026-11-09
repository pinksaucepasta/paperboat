//go:build windows

package localapi

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestWindowsControlWatcherCancellationJoinsBlockedReader(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); watchControlHangup(ctx, local, Peer{}, cancel) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("control watcher retained its blocked reader")
	}
	if _, err := remote.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("control cancellation did not close the dedicated connection")
	}
}

// The end of either direction ends the whole stream, also while the client's process lives on: before, a client
// that dropped a stream kept its remote open until the process exited, and a client still reading never saw a
// remote stream end (pb exec then waited until its own timeout).
func TestWindowsBridgeEndsBothSidesWhenEitherSideEnds(t *testing.T) {
	for _, side := range []string{"local", "remote"} {
		t.Run(side+" ends", func(t *testing.T) {
			local, client := net.Pipe()
			remote, far := net.Pipe()
			defer client.Close()
			defer far.Close()
			done := make(chan struct{})
			go func() { defer close(done); bridgePeerStream(t.Context(), local, remote) }()
			ending, other := client, far
			if side == "remote" {
				ending, other = far, client
			}
			if err := ending.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the bridge kept running after one side ended")
			}
			if _, err := other.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("the other side was left open: %v", err)
			}
		})
	}
}
