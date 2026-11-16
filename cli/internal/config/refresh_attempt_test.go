package config

import (
	"errors"
	"testing"
	"time"
)

type noLock struct{}

func (noLock) Lock() error   { return nil }
func (noLock) Unlock() error { return nil }

// staleReadSecretStore hands out the refresh token it read, then lets another holder rotate and commit before the
// caller continues: what a process sees when a second process takes the credential lock it still holds.
type staleReadSecretStore struct {
	faultSecretStore
	ref   string
	other func()
}

func (s *staleReadSecretStore) Get(ref string) (string, error) {
	value, err := s.faultSecretStore.Get(ref)
	if ref == s.ref && s.other != nil {
		other := s.other
		s.other = nil
		other()
	}
	return value, err
}

func TestRefreshNeverPairsConsumedTokenWithFreshAttempt(t *testing.T) {
	secrets := &staleReadSecretStore{faultSecretStore: faultSecretStore{values: map[string]string{}}}
	store := ProfileStore{Path: t.TempDir(), Secrets: secrets}
	issuer := "https://api.example.com"
	expired := time.Now().Add(-time.Minute)
	if err := store.Save(Profile{Issuer: issuer, CLIClientSessionID: "cls_1", AccessExpiresAt: expired}, Credential{AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: expired}); err != nil {
		t.Fatal(err)
	}
	profile, err := store.Load(issuer)
	if err != nil {
		t.Fatal(err)
	}
	secrets.ref = profile.RefreshSecretRef
	secrets.other = func() {
		if _, err := store.credentialWithRefresh(issuer, time.Minute, func(Credential, string) (Credential, string, error) {
			return Credential{AccessToken: "access-a", RefreshToken: "refresh-a", ExpiresAt: expired}, "cls_1", nil
		}, noLock{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.credentialWithRefresh(issuer, time.Minute, func(original Credential, _ string) (Credential, string, error) {
		if original.RefreshToken != "refresh-a" {
			t.Fatalf("sent consumed refresh token %q with a fresh attempt id: the server revokes that as replay", original.RefreshToken)
		}
		return Credential{AccessToken: "access-b", RefreshToken: "refresh-b", ExpiresAt: time.Now().Add(time.Hour)}, "cls_1", nil
	}, noLock{}); err != nil {
		t.Fatal(err)
	}
}

// A process killed after the server rotated (response lost) leaves its journal; the next refresh resends the same
// original token with the same attempt id, which the server answers with the stored successor.
func TestRefreshAfterLostResponseResendsSameAttempt(t *testing.T) {
	secrets := &faultSecretStore{values: map[string]string{}}
	store := ProfileStore{Path: t.TempDir(), Secrets: secrets}
	issuer := "https://api.example.com"
	expired := time.Now().Add(-time.Minute)
	if err := store.Save(Profile{Issuer: issuer, CLIClientSessionID: "cls_1", AccessExpiresAt: expired}, Credential{AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: expired}); err != nil {
		t.Fatal(err)
	}
	var first string
	if _, err := store.CredentialWithRefresh(issuer, time.Minute, func(_ Credential, attempt string) (Credential, string, error) {
		first = attempt
		return Credential{}, "", errors.New("context deadline exceeded after the server rotated")
	}); err == nil {
		t.Fatal("expected the lost response to fail the refresh")
	}
	got, err := store.CredentialWithRefresh(issuer, time.Minute, func(original Credential, attempt string) (Credential, string, error) {
		if original.RefreshToken != "refresh-old" || attempt != first {
			t.Fatalf("retry sent token %q attempt %q, want the journaled pair", original.RefreshToken, attempt)
		}
		return Credential{AccessToken: "access-new", RefreshToken: "refresh-new", ExpiresAt: time.Now().Add(time.Hour)}, "cls_1", nil
	})
	if err != nil || got.RefreshToken != "refresh-new" {
		t.Fatalf("retry = %q, %v", got.RefreshToken, err)
	}
	if _, err := store.loadRefreshAttempt(issuer); err == nil {
		t.Fatal("committed attempt journal left behind")
	}
}
