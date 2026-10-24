package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"strings"
	"testing"
	"time"
)

type maxAgeFetcher struct {
	body   func() string
	maxAge time.Duration
}

func (f maxAgeFetcher) Fetch(context.Context) (io.ReadCloser, time.Duration, error) {
	return io.NopCloser(strings.NewReader(f.body())), f.maxAge, nil
}

// F21-18: the server says how long its key set stays current (Cache-Control max-age, one hour), bounded to
// a minute..an hour; without it the configured lifetime applies.
func TestCacheControlMaxAgeIsBounded(t *testing.T) {
	for header, want := range map[string]time.Duration{
		"public, max-age=3600": time.Hour, "max-age=86400": time.Hour, "max-age=5": time.Minute,
		"no-store": 0, "max-age=abc": 0, "": 0, "MAX-AGE=120, public": 2 * time.Minute,
	} {
		if got := cacheMaxAge(header); got != want {
			t.Errorf("%q: %v, want %v", header, got, want)
		}
	}
}

// A key set read with max-age=3600 stays current for an hour and is re-read at half of it; a withdrawn key
// stops verifying at the next read, while a key that only left the set stays for the credentials it signed.
func TestJWKSFollowsMaxAgeAndDropsWithdrawnKeys(t *testing.T) {
	clock := &fixedClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	key := func(b byte) string {
		k := make(ed25519.PublicKey, ed25519.PublicKeySize)
		k[0] = b
		return base64.RawURLEncoding.EncodeToString(k)
	}
	entry := func(id string, b byte) string {
		return `{"kty":"OKP","crv":"Ed25519","use":"sig","alg":"EdDSA","kid":"` + id + `","x":"` + key(b) + `"}`
	}
	document := `{"keys":[` + entry("old", 1) + `,` + entry("compromised", 2) + `]}`
	cache, err := NewJWKSCache(JWKSConfig{Clock: clock, TTL: 5 * time.Minute, Fetcher: maxAgeFetcher{body: func() string { return document }, maxAge: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := cache.RefreshInterval(); got != 30*time.Minute {
		t.Fatalf("refresh interval %v, want half the server's hour", got)
	}
	clock.now = clock.now.Add(50 * time.Minute)
	if _, ok, _ := cache.Lookup(t.Context(), "old"); !ok {
		t.Fatal("a key read with max-age=3600 expired after 50 min")
	}
	document = `{"keys":[` + entry("new", 3) + `],"withdrawn_key_ids":["compromised"]}`
	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := cache.Lookup(t.Context(), "compromised"); ok {
		t.Fatal("a withdrawn key still verifies")
	}
	if _, ok, _ := cache.Lookup(t.Context(), "old"); !ok {
		t.Fatal("a key that only left the set was dropped instead of retained")
	}
	if _, ok, _ := cache.Lookup(t.Context(), "new"); !ok {
		t.Fatal("the rotated-in key is unknown")
	}
	document = `{"keys":[` + entry("new", 3) + `],"withdrawn_key_ids":["new"]}`
	clock.now = clock.now.Add(time.Minute)
	if err := cache.Refresh(t.Context()); err == nil {
		t.Fatal("a set that withdraws one of its own keys was accepted")
	}
}
