package config

import (
	"errors"
	"testing"
	"time"
)

func TestSecretCacheReadsOnceWithinTTLAndSeesOwnWrites(t *testing.T) {
	now := time.Unix(1000, 0)
	cache := &secretCache{ttl: 30 * time.Second, now: func() time.Time { return now }}
	reads := 0
	read := func() (string, error) { reads++; return "v1", nil }
	for range 5 {
		if value, err := cache.get("ref", read); err != nil || value != "v1" {
			t.Fatalf("get = %q, %v", value, err)
		}
	}
	if reads != 1 {
		t.Fatalf("keychain read %d times within the ttl, want 1", reads)
	}
	cache.set("ref", "v2")
	if value, _ := cache.get("ref", read); value != "v2" || reads != 1 {
		t.Fatalf("own write not seen: %q after %d reads", value, reads)
	}
	cache.drop("ref")
	if value, _ := cache.get("ref", read); value != "v1" || reads != 2 {
		t.Fatalf("a dropped entry was served from the cache: %q after %d reads", value, reads)
	}
	now = now.Add(31 * time.Second)
	cache.get("ref", read)
	if reads != 3 {
		t.Fatalf("an expired entry was not read again (%d reads)", reads)
	}
}

func TestSecretCacheDoesNotRememberErrors(t *testing.T) {
	cache := &secretCache{ttl: time.Hour, now: time.Now}
	fail := errors.New("locked")
	if _, err := cache.get("ref", func() (string, error) { return "", fail }); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	if value, err := cache.get("ref", func() (string, error) { return "ok", nil }); err != nil || value != "ok" {
		t.Fatalf("an error was remembered: %q, %v", value, err)
	}
}

// Another process rotates the refresh token under the same reference; a cached copy would be sent with a fresh
// attempt id and revoke the session, so every read of a refresh token reaches the keychain.
func TestSecretCacheNeverServesRefreshTokens(t *testing.T) {
	cache := &secretCache{ttl: time.Hour, now: time.Now}
	for _, ref := range []string{"profile-abc-refresh", "refresh-attempt-abc-0123"} {
		keychain := "r0"
		read := func() (string, error) { return keychain, nil }
		cache.get(ref, read)
		cache.set(ref, "r0")
		keychain = "r1"
		if value, _ := cache.get(ref, read); value != "r1" {
			t.Fatalf("%s served a cached refresh token %q after another process rotated it", ref, value)
		}
	}
	keychain := "a0"
	cache.get("profile-abc-access", func() (string, error) { return keychain, nil })
	keychain = "a1"
	if value, _ := cache.get("profile-abc-access", func() (string, error) { return keychain, nil }); value != "a0" {
		t.Fatal("access tokens stopped being cached")
	}
}
