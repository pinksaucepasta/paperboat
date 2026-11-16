package localdaemon

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestClientConfigurationIsReadOncePerTTLAndAFailureIsNotCached(t *testing.T) {
	calls, fail := 0, true
	now := time.Unix(0, 0)
	read := cachedClientConfiguration(func(context.Context) (string, error) {
		calls++
		if fail {
			return "", errors.New("unreachable")
		}
		return "example.test", nil
	}, func() time.Time { return now })
	if _, err := read(context.Background()); err == nil {
		t.Fatal("a failed read returned no error")
	}
	fail = false
	for range 3 {
		if got, err := read(context.Background()); err != nil || got != "example.test" {
			t.Fatalf("read = %q, %v", got, err)
		}
	}
	if calls != 2 {
		t.Fatalf("server reads = %d, want 1 failed + 1 cached", calls)
	}
	now = now.Add(clientConfigurationTTL)
	_, _ = read(context.Background())
	if calls != 3 {
		t.Fatalf("a read after the TTL did not ask again: %d", calls)
	}
}
