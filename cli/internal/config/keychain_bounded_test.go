package config

import (
	"errors"
	"testing"
	"time"
)

// A Keychain call that never answers ends with the credential-store refusal instead of hanging `pb pair` (X-28).
func TestBoundedKeychainGivesUpOnACallThatNeverAnswers(t *testing.T) {
	previous := keychainCallTimeout
	keychainCallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { keychainCallTimeout = previous })
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	started := time.Now()
	_, err := boundedKeychain(func() (string, error) { <-block; return "late", nil })
	if !errors.Is(err, ErrCredentialStoreUnavailable) || time.Since(started) > 5*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(started))
	}
	want := errors.New("not found")
	if value, err := boundedKeychain(func() (string, error) { return "v", want }); value != "v" || err != want {
		t.Fatalf("a prompt answer was changed: %q %v", value, err)
	}
}
