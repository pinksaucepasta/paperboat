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
