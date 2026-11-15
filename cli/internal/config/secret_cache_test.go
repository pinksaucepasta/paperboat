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
