package derpquic

import (
	"context"
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
