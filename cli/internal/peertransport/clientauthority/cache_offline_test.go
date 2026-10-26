package clientauthority

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// With Paperboat unreachable past the stale window, the verified local identity is still served
// while its certificate lasts, so offline sessions keep starting (F21-11); a refusal is final.
func TestResolveLocalKeepsTheVerifiedIdentityWhilePaperboatIsUnreachable(t *testing.T) {
	cache := NewCache()
	var answer error
	cache.resolveLocal = func(_ context.Context, request Request) (Authority, error) {
		if answer != nil {
			return Authority{}, answer
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
	answer = &url.Error{Op: "Post", URL: "https://api.example.test", Err: errors.New("connection refused")}
	if got, err := resolve(10 * time.Minute); err != nil || got.RootPublic[0] != 1 {
		t.Fatalf("unreachable server 10 min in: %v", err)
	}
	if _, err := resolve(2 * time.Hour); err == nil {
		t.Fatal("an expired local certificate was served")
	}
	answer = &api.APIError{Status: 403, Code: "forbidden"}
	if _, err := resolve(10 * time.Minute); err == nil {
		t.Fatal("a refusal was answered from the cache")
	}
}
