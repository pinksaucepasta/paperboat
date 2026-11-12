package clientauthority

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// With Paperboat unreachable the verified local identity is still served while its certificate
// lasts, so offline sessions keep starting (F21-11), and a server that does not answer at all (a
// dropped port: the request hangs) never blocks the caller (F21-01). A refusal the server answers
// drops the identity: the next resolution reports it.
func TestResolveLocalKeepsTheVerifiedIdentityWhilePaperboatIsUnreachable(t *testing.T) {
	cache := NewCache()
	var answerMu sync.Mutex
	var answer error
	setAnswer := func(err error) { answerMu.Lock(); answer = err; answerMu.Unlock() }
	var hang atomic.Bool
	released := make(chan struct{})
	cache.resolveLocal = func(ctx context.Context, request Request) (Authority, error) {
		if hang.Load() {
			select {
			case <-ctx.Done():
				return Authority{}, ctx.Err()
			case <-released:
				return Authority{}, context.DeadlineExceeded
			}
		}
		answerMu.Lock()
		err := answer
		answerMu.Unlock()
		if err != nil {
			return Authority{}, err
		}
		authority := Authority{RootPublic: ed25519.PublicKey{1}}
		authority.LocalCertificate.Claims.ExpiresAt = request.Now.Add(time.Hour)
		return authority, nil
	}
	start := time.Now().UTC()
	resolve := func(at time.Duration) (Authority, error) {
		return cache.ResolveLocal(context.Background(), Request{AccountID: "account", CLIClientSessionID: "session", Now: start.Add(at)})
	}
	if _, err := resolve(0); err != nil {
		t.Fatal(err)
	}
	hang.Store(true)
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := cache.ResolveLocal(ctx, Request{AccountID: "account", CLIClientSessionID: "session", Now: start.Add(4 * time.Minute)})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a hanging server 4 min in: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a hanging server blocked the resolution of a verified identity")
	}
	hang.Store(false)
	close(released) // the background refresh that hung gives up
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		cache.mu.Lock()
		idle := len(cache.refreshing) == 0
		cache.mu.Unlock()
		if idle {
			break
		}
	}
	setAnswer(&url.Error{Op: "Post", URL: "https://api.example.test", Err: errors.New("connection refused")})
	if got, err := resolve(10 * time.Minute); err != nil || got.RootPublic[0] != 1 {
		t.Fatalf("unreachable server 10 min in: %v", err)
	}
	if _, err := resolve(2 * time.Hour); err == nil {
		t.Fatal("an expired local certificate was served")
	}
	setAnswer(&api.APIError{Status: 429, Code: "rate_limited"}) // a busy server is not a refusal
	resolve(10 * time.Minute)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		cache.mu.Lock()
		idle := len(cache.refreshing) == 0
		cache.mu.Unlock()
		if idle {
			break
		}
	}
	if _, err := resolve(10*time.Minute + time.Second); err != nil {
		t.Fatalf("a 429 dropped the verified identity: %v", err)
	}
	setAnswer(&api.APIError{Status: 403, Code: "forbidden"})
	resolve(10 * time.Minute) // still served; its background refresh learns the refusal
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := resolve(11 * time.Minute); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a refusal the server answered left the identity in the cache")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
