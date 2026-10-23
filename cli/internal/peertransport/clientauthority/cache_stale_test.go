package clientauthority

import (
	"context"
	"crypto/ed25519"
	"sync/atomic"
	"testing"
	"time"
)

// A command after a short idle gap must not wait for two control-plane round trips: a stale
// local identity is served while exactly one background refresh replaces it, and only an entry
// past localServeStaleFor makes the caller wait for the server.
func TestResolveLocalServesStaleIdentityWhileOneRefreshRuns(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	cache := NewCache()
	cache.resolveLocal = func(ctx context.Context, request Request) (Authority, error) {
		if calls.Add(1) > 1 {
			select { // hold background refreshes until the test lets them finish
			case <-release:
			case <-ctx.Done():
				return Authority{}, ctx.Err()
			}
		}
		authority := Authority{RootPublic: ed25519.PublicKey{byte(calls.Load())}}
		authority.LocalCertificate.Claims.ExpiresAt = request.Now.Add(24 * time.Hour)
		return authority, nil
	}
	start := time.Now().UTC()
	request := func(at time.Duration) Request {
		return Request{AccountID: "account", CLIClientSessionID: "session", Now: start.Add(at)}
	}
	resolve := func(at time.Duration) Authority {
		t.Helper()
		authority, err := cache.ResolveLocal(context.Background(), request(at))
		if err != nil {
			t.Fatal(err)
		}
		return authority
	}
	resolve(0)
	resolve(10 * time.Second)
	if got := calls.Load(); got != 1 {
		t.Fatalf("a fresh identity was resolved %d times", got)
	}
	if got := resolve(20 * time.Second); got.RootPublic[0] != 1 {
		t.Fatal("a stale identity was not served from the cache")
	}
	resolve(25 * time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("stale reads started %d background refreshes, want exactly 1", got-1)
	}
	close(release)
	for deadline = time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		cache.mu.Lock()
		done := len(cache.refreshing) == 0
		cache.mu.Unlock()
		if done {
			break
		}
	}
	before := calls.Load()
	resolve(start.Sub(start) + localServeStaleFor + time.Hour)
	if calls.Load() != before+1 {
		t.Fatal("an identity past localServeStaleFor was served without waiting for the server")
	}
}
