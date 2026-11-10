package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

type failNthGet struct {
	config.SecretStore
	gets, fail int
}

func (s *failNthGet) Get(ref string) (string, error) {
	s.gets++
	if s.gets == s.fail {
		return "", config.ErrCredentialStoreUnavailable
	}
	return s.SecretStore.Get(ref)
}

// One failed read of the local credential store (on Windows, a lost lock race) is not remembered by the daemon's
// source: nothing reached Paperboat, so the next peer operation reads the store again instead of failing from the
// refresh backoff for 15 s to 5 min (victus 2026-11-10: every pb ping and pb exec failed).
func TestLocalCredentialStoreFailureIsNotRemembered(t *testing.T) {
	store, profile := credentialFailureFixture(t, "https://api.example.test")
	// Gets 1 and 2 are the source's own read; 3 is the first read under the store lock.
	store.Secrets = &failNthGet{SecretStore: store.Secrets, fail: 3}
	source := (&Source{Store: store, Issuer: profile.Issuer}).WithContext(context.Background())
	if _, err := source.Credential(); !errors.Is(err, config.ErrCredentialStoreUnavailable) {
		t.Fatalf("store failure = %v", err)
	}
	credential, err := source.Credential()
	if err != nil || credential.AccessToken != "access-old" {
		t.Fatalf("the store works again but the source still fails: %v", err)
	}
}
