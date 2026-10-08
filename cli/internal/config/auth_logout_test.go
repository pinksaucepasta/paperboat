package config

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestAuthenticationCleanupPreservesPasswordVaultCustody(t *testing.T) {
	for _, action := range []string{"remove", "queue_active", "complete", "discard_all", "replace"} {
		t.Run(action, func(t *testing.T) {
			store := ProfileStore{Path: t.TempDir(), Secrets: &faultSecretStore{values: map[string]string{}}}
			issuer, account, session := "https://api.example.com", "account_1", "cls_1"
			if err := store.Save(Profile{Issuer: issuer, Account: Account{ID: account}, CLIClientSessionID: session}, Credential{AccessToken: "test-access", RefreshToken: "test-refresh"}); err != nil {
				t.Fatal(err)
			}
			ref := passwordVaultSecretRef(issuer, account)
			if err := store.Secrets.Set(ref, "test-encrypted-key-record"); err != nil {
				t.Fatal(err)
			}
			var err error
			switch action {
			case "remove":
				_, err = store.Remove(issuer)
			case "queue_active":
				err = store.QueueActiveRevocation(issuer)
			case "replace":
				err = store.Replace(Profile{Issuer: issuer, Account: Account{ID: account}, CLIClientSessionID: "cls_2"}, Credential{AccessToken: "replacement-access", RefreshToken: "replacement-refresh"})
			default:
				if err = store.QueueRevocation(issuer, "cls_old", "test-refresh-old", account); err != nil {
					t.Fatal(err)
				}
				ref = passwordVaultSecretRef(issuer, account)
				if err = store.Secrets.Set(ref, "test-encrypted-key-record"); err != nil {
					t.Fatal(err)
				}
				if action == "discard_all" {
					err = store.DiscardPendingRevocations(issuer)
				} else {
					records, loadErr := store.PendingRevocations(issuer)
					if loadErr != nil || len(records) != 1 {
						t.Fatal("missing revocation fixture")
					}
					err = store.CompleteRevocation(records[0])
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Secrets.Get(ref); err != nil {
				t.Fatal("authentication cleanup destroyed ENV custody")
			}
		})
	}
}

func TestTakeLogoutCredentialsAtomicallyRemovesActiveAndHistoricalSessions(t *testing.T) {
	dir := t.TempDir()
	store := ProfileStore{Path: dir, Secrets: &faultSecretStore{values: map[string]string{}}}
	issuer := "https://api.example.com"
	accountID := "account_active"
	if err := store.Save(Profile{Issuer: issuer, Account: Account{ID: accountID}, CLIClientSessionID: "cls_active"}, Credential{AccessToken: "access-active", RefreshToken: "refresh-active"}); err != nil {
		t.Fatal(err)
	}
	environmentRef := passwordVaultSecretRef(issuer, accountID)
	store.Secrets.(*faultSecretStore).values[environmentRef] = "encrypted-vault-record"
	if err := store.QueueRevocation(issuer, "cls_old", "refresh-old"); err != nil {
		t.Fatal(err)
	}
	credentials, err := store.TakeLogoutCredentials(issuer)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, credential := range credentials {
		got[credential.RefreshToken] = true
	}
	if !got["refresh-active"] || !got["refresh-old"] || len(got) != 2 {
		t.Fatalf("logout credentials = %#v", got)
	}
	if _, err := store.Load(issuer); err != ErrNoCredentials {
		t.Fatalf("profile remains after logout: %v", err)
	}
	if records, err := store.PendingRevocations(issuer); err != nil || len(records) != 0 {
		t.Fatalf("pending revocations remain: %#v, %v", records, err)
	}
	if _, ok := store.Secrets.(*faultSecretStore).values[environmentRef]; !ok {
		t.Fatal("logout destroyed ENV decryption custody")
	}
}

func TestTakeLogoutCredentialsRemovesBrokenProfileWithoutInventingToken(t *testing.T) {
	dir := t.TempDir()
	secrets := &faultSecretStore{values: map[string]string{}}
	store := ProfileStore{Path: dir, Secrets: secrets}
	issuer := "https://api.example.com"
	if err := store.Save(Profile{Issuer: issuer, CLIClientSessionID: "cls_active"}, Credential{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	profile, _ := store.Load(issuer)
	delete(secrets.values, profile.RefreshSecretRef)
	credentials, err := store.TakeLogoutCredentials(issuer)
	partial, ok := err.(*LogoutRevocationIncompleteError)
	if !ok || partial.UnreadableCredentials() != 1 || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing read failure/local signout evidence")
	}
	if len(credentials) != 0 {
		t.Fatalf("invented broken-profile credential: %#v", credentials)
	}
	if _, err := store.Load(issuer); err != ErrNoCredentials {
		t.Fatalf("broken profile remains after logout: %v", err)
	}
}

func TestTakeLogoutMalformedHistoricalRecordKeepsReadableCurrentTokenAndCause(t *testing.T) {
	store := ProfileStore{Path: t.TempDir(), Secrets: &faultSecretStore{values: map[string]string{}}}
	issuer := "https://api.example.com"
	if err := store.Save(Profile{Issuer: issuer, CLIClientSessionID: "cls_active"}, Credential{AccessToken: "PRIVATE_ACCESS", RefreshToken: "PRIVATE_REFRESH"}); err != nil {
		t.Fatal(err)
	}
	if err := store.QueueRevocation(issuer, "cls_old", "PRIVATE_OLD"); err != nil {
		t.Fatal(err)
	}
	path := store.existingPendingRevocationPath(issuer, "cls_old")
	if err := os.WriteFile(path, []byte(`{"PRIVATE":`), 0600); err != nil {
		t.Fatal(err)
	}
	credentials, err := store.TakeLogoutCredentials(issuer)
	partial, ok := err.(*LogoutRevocationIncompleteError)
	var syntax *json.SyntaxError
	if !ok || partial.UnreadableCredentials() != 1 || !errors.As(err, &syntax) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("partial logout discarded original parser cause or exposed content")
	}
	if len(credentials) != 1 || credentials[0].RefreshToken != "PRIVATE_REFRESH" {
		t.Fatal("readable current credential was lost")
	}
	if _, err := store.Load(issuer); !errors.Is(err, ErrNoCredentials) {
		t.Fatal("local signout did not complete")
	}
	records, err := store.PendingRevocations(issuer)
	if err != nil || len(records) != 0 {
		t.Fatal("logout retained historical records")
	}
}

func TestTakeLogoutReadAndCleanupFailureRetainBothWithoutCompletionClaim(t *testing.T) {
	secrets := &faultSecretStore{values: map[string]string{}}
	store := ProfileStore{Path: t.TempDir(), Secrets: secrets}
	issuer := "https://api.example.com"
	if err := store.Save(Profile{Issuer: issuer, CLIClientSessionID: "cls_active"}, Credential{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	profile, err := store.Load(issuer)
	if err != nil {
		t.Fatal(err)
	}
	delete(secrets.values, profile.RefreshSecretRef)
	secrets.failDeleteRef = profile.AccessSecretRef
	_, err = store.TakeLogoutCredentials(issuer)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup failure discarded the original credential read cause")
	}
	var completed *LogoutRevocationIncompleteError
	if errors.As(err, &completed) {
		t.Fatal("incomplete local cleanup claimed local signout completed")
	}
}
