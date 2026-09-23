package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func enrollmentLoginTestStore(t *testing.T) (ProfileStore, FileSecretStore) {
	t.Helper()
	root := t.TempDir()
	secrets := FileSecretStore{Dir: filepath.Join(root, "secrets")}
	return ProfileStore{Path: root, Secrets: secrets}, secrets
}

func enrollmentLoginTestVerifier() string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, enrollmentLoginVerifierBytes))
}

func TestWithEnrollmentLoginRoundTripsRecoveryAndIssuedResponse(t *testing.T) {
	store, secrets := enrollmentLoginTestStore(t)
	issuer := "https://API.example.test/"
	token := "A1234567890123456789012345"
	verifier := enrollmentLoginTestVerifier()
	firstErr := errors.New("exchange interrupted")
	if err := store.WithEnrollmentLogin(issuer, func(state *EnrollmentLoginState, save func() error) error {
		state.Token, state.Verifier = token, verifier
		if err := save(); err != nil {
			return err
		}
		return firstErr
	}); !errors.Is(err, firstErr) {
		t.Fatalf("first callback error = %v", err)
	}

	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	profile := Profile{Issuer: issuer, Account: Account{ID: "account_1", Email: "user@example.test"}, CLIClientSessionID: "session_1", AccessExpiresAt: expiresAt}
	credential := Credential{AccessToken: "access-token", RefreshToken: "refresh-token", TokenType: "Bearer", ExpiresAt: expiresAt}
	if err := store.WithEnrollmentLogin(issuer, func(state *EnrollmentLoginState, save func() error) error {
		if state.Token != token || state.Verifier != verifier {
			t.Fatalf("resumed token/verifier = %q/%q", state.Token, state.Verifier)
		}
		state.Cancelled = true
		if err := save(); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithEnrollmentLogin(issuer, func(state *EnrollmentLoginState, save func() error) error {
		if state.Token != token || state.Verifier != verifier || !state.Cancelled || state.Profile != nil {
			t.Fatalf("resumed cancelled state = %#v", *state)
		}
		state.Profile, state.Credential = &profile, &credential
		return save()
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithEnrollmentLogin(issuer, func(state *EnrollmentLoginState, save func() error) error {
		if state.Token != token || state.Verifier != verifier || !state.Cancelled || state.Profile == nil || state.Credential == nil {
			t.Fatalf("resumed issued state = %#v", *state)
		}
		if state.Profile.Issuer != issuer || state.Profile.CLIClientSessionID != profile.CLIClientSessionID || state.Credential.RefreshToken != credential.RefreshToken {
			t.Fatalf("resumed response = %#v/%#v", *state.Profile, *state.Credential)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Get(enrollmentLoginSecretRef(issuer)); err != nil {
		t.Fatalf("recovery record missing after round trip: %v", err)
	}
}

func TestWithEnrollmentLoginEmptyStateDeletesRecovery(t *testing.T) {
	store, secrets := enrollmentLoginTestStore(t)
	issuer := "https://api.example.test"
	if err := store.WithEnrollmentLogin(issuer, func(state *EnrollmentLoginState, save func() error) error {
		state.Token, state.Verifier = "A1234567890123456789012345", enrollmentLoginTestVerifier()
		return save()
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithEnrollmentLogin(issuer, func(state *EnrollmentLoginState, save func() error) error {
		*state = EnrollmentLoginState{}
		return save()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Get(enrollmentLoginSecretRef(issuer)); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("recovery record error after clear = %v", err)
	}
}

func TestWithEnrollmentLoginIsolatesIssuers(t *testing.T) {
	store, secrets := enrollmentLoginTestStore(t)
	issuers := []string{"https://one.example.test", "https://two.example.test"}
	verifiers := []string{
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, enrollmentLoginVerifierBytes)),
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, enrollmentLoginVerifierBytes)),
	}
	for i, issuer := range issuers {
		issuer, verifier := issuer, verifiers[i]
		if err := store.WithEnrollmentLogin(issuer, func(state *EnrollmentLoginState, save func() error) error {
			state.Token, state.Verifier = "A1234567890123456789012345", verifier
			return save()
		}); err != nil {
			t.Fatal(err)
		}
	}
	if enrollmentLoginSecretRef(issuers[0]) == enrollmentLoginSecretRef(issuers[1]) {
		t.Fatal("issuer recovery references collided")
	}
	for i, issuer := range issuers {
		want := verifiers[i]
		if err := store.WithEnrollmentLogin(issuer, func(state *EnrollmentLoginState, save func() error) error {
			if state.Verifier != want {
				t.Fatalf("issuer %q verifier = %q, want %q", issuer, state.Verifier, want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := secrets.Get(enrollmentLoginSecretRef(issuers[0])); err != nil {
		t.Fatal(err)
	}
}

func TestWithEnrollmentLoginRejectsCorruptRecordWithoutSecretContent(t *testing.T) {
	store, secrets := enrollmentLoginTestStore(t)
	issuer := "https://api.example.test"
	secret := "A1234567890123456789012345"
	if err := secrets.Set(enrollmentLoginSecretRef(issuer), `{"version":1,"kind":"enrollment_login","issuer":"https://api.example.test","token":"`+secret+`"}`); err != nil {
		t.Fatal(err)
	}
	err := store.WithEnrollmentLogin(issuer, func(*EnrollmentLoginState, func() error) error {
		t.Fatal("corrupt state reached callback")
		return nil
	})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("corrupt record error = %v", err)
	}
}

func TestWithEnrollmentLoginConcurrentInitializationKeepsFirstVerifier(t *testing.T) {
	store, _ := enrollmentLoginTestStore(t)
	issuer := "https://api.example.test"
	const callers = 16
	observed := make(chan string, callers)
	var wait sync.WaitGroup
	for i := 0; i < callers; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			err := store.WithEnrollmentLogin(issuer, func(state *EnrollmentLoginState, save func() error) error {
				if state.Token == "" {
					state.Token = "A1234567890123456789012345"
					state.Verifier = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{byte(i + 1)}, enrollmentLoginVerifierBytes))
					if err := save(); err != nil {
						return err
					}
				}
				observed <- state.Verifier
				return nil
			})
			if err != nil {
				t.Errorf("callback %d: %v", i, err)
			}
		}(i)
	}
	wait.Wait()
	close(observed)
	var first string
	for verifier := range observed {
		if first == "" {
			first = verifier
		} else if verifier != first {
			t.Fatalf("concurrent callbacks observed verifier %q after %q", verifier, first)
		}
	}
	if first == "" {
		t.Fatal("no verifier observed")
	}
	if _, err := os.Stat(filepath.Join(store.Path, "profiles")); err != nil {
		t.Fatal(err)
	}
}
