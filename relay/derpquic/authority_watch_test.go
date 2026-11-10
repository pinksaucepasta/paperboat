package derpquic

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// Each grant a carrier sends costs the relay a full verification under its lock. Re-sending an
// unchanged grant every second stalled admissions near 1,000 connections on one node.
func TestCarrierSendsOnlyChangedGrants(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var token atomic.Value
	token.Store("grant-1")
	var sends atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchCarrierAuthority(ctx, func(context.Context) (string, error) { return token.Load().(string), nil },
			func(string) error { sends.Add(1); return nil }, func() {})
	}()
	time.Sleep(3500 * time.Millisecond)
	if got := sends.Load(); got != 1 {
		t.Fatalf("%d sends of an unchanged grant in 3.5 s, want 1", got)
	}
	token.Store("grant-2")
	deadline := time.Now().Add(3 * time.Second)
	for sends.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := sends.Load(); got != 2 {
		t.Fatalf("a changed grant was not sent: %d sends", got)
	}
	cancel()
	<-done
}

// A grant that did not reach the relay is a network failure: it is sent again, and the carrier is not revoked
// (revoking closes a QUIC connection with the admission code, which latches the carrier fatal until a restart).
// A grant the client cannot encode is still revoked.
func TestFailedReauthenticationInTheNetworkIsRetriedNotRevoked(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var sends, revokes atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchCarrierAuthority(ctx, func(context.Context) (string, error) { return "grant", nil },
			func(string) error {
				if sends.Add(1) == 1 {
					return fmt.Errorf("%w: write: connection reset", errAuthorizeTransport)
				}
				return nil
			}, func() { revokes.Add(1) })
	}()
	deadline := time.Now().Add(6 * time.Second)
	for sends.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sends.Load() < 2 || revokes.Load() != 0 {
		t.Fatalf("%d sends, %d revocations: the undelivered grant must be sent again without revoking", sends.Load(), revokes.Load())
	}
	cancel()
	<-done

	revoked := make(chan struct{})
	go watchCarrierAuthority(t.Context(), func(context.Context) (string, error) { return "garbled", nil },
		func(string) error { return errors.New("grant cannot be encoded") }, func() { close(revoked) })
	select {
	case <-revoked:
	case <-time.After(5 * time.Second):
		t.Fatal("a grant the client cannot encode was not revoked")
	}
}
