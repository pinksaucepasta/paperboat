package native_test

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"tailscale.com/tailcfg"
)

// The wake dial and terminal reconnect own the same peer engine. Neither is an
// admitted connection while its QUIC handshake is pending.
func testConcurrentRestart(t *testing.T, parent context.Context, owner *native.Owner, receiver *tailnet.Authority, region *tailcfg.DERPRegion, tls *tls.Config, address mesh.Addr, stop func()) {
	t.Helper()
	first, err := owner.Dial(parent, address, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := first.Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := stream.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := stream.Read(b[:]); err != nil || b[0] != 'x' {
		t.Fatal("warm peer echo failed", err)
	}
	_ = stream.Close()
	_ = first.Close()
	stop()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	results := make(chan error, 2)
	dial := func() {
		s, e := owner.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
		if s != nil {
			_ = s.Close()
		}
		results <- e
	}
	go dial()
	time.Sleep(200 * time.Millisecond)
	go dial()
	// Cancelling a queued caller must release only its own claim.
	waiter, cancelWaiter := context.WithCancel(ctx)
	cancelled := make(chan error, 1)
	go func() {
		s, err := owner.Dial(waiter, address, "machine_test", peerquic.ClassInteractive)
		if s != nil {
			_ = s.Close()
		}
		cancelled <- err
	}()
	cancelWaiter()
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("queued cancellation changed", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("queued cancellation blocked")
	}
	// Both attempts have sent into the old engine before its replacement starts.
	time.Sleep(time.Second)
	_, _, _, closeNew := listenWithLostHandshake(t, ctx, receiver, region, tls, 0)
	defer closeNew()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal("reconnect failed", err)
			}
		case <-timer.C:
			t.Fatal("overlapping pending dials kept restarted peer unreachable more than1s")
		}
	}
}
