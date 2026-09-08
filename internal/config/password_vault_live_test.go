package config

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

// Run explicitly on the designated native client. No existing account/profile
// is accessed, and every task-owned secure-store object is removed afterward.
func TestPasswordVaultProductionSecureCustody(t *testing.T) {
	if os.Getenv("PAPERBOAT_TEST_ENV_CUSTODY") != "1" {
		t.Skip("native secure custody check is not requested")
	}
	root := filepath.Join(t.TempDir(), "secure-root")
	if err := ensureProfileDirectory(root); err != nil {
		t.Fatal(err)
	}
	store := ProfileStore{Path: filepath.Join(root, "profiles.json"), Secrets: KeyringStore{}}
	issuer := "https://vault-custody.invalid"
	account := fmt.Sprintf("custody_%d", time.Now().UnixNano())
	if err := store.RequireEnvironmentSecureStore(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.RemovePasswordVault(issuer, account); err != nil {
			t.Errorf("remove task custody: %v", err)
		}
	}()
	keys, err := environmente2ee.NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Clear()
	for i := 0; i < environmente2ee.MaximumVaultTeams; i++ {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		keys.Teams = append(keys.Teams, environmente2ee.VaultTeamKey{TeamID: fmt.Sprintf("team_%03d", i), Epoch: 1, MembershipGeneration: 1, Key: key})
	}
	payload, err := keys.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(payload)
	raw, head, err := environmente2ee.SealPasswordVault(context.Background(), environmente2ee.VaultHead{Issuer: issuer, AccountID: account, Generation: 1}, environmente2ee.DocumentID{}, []byte("native custody test password"), payload)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string][]byte{}
	defer func() {
		for _, value := range values {
			clear(value)
		}
	}()
	for i := 0; i < 8; i++ {
		values[fmt.Sprintf("ENV_TEST_%d", i)] = bytes.Repeat([]byte("x"), 32000)
	}
	scope, err := environmente2ee.SealVaultScope(context.Background(), environmente2ee.VaultScopeClaims{Issuer: issuer, OwnerKind: "personal", OwnerID: account, KeyEpoch: 1, Revision: 1, Previous: make([]byte, 32), WriterAccount: account, WriterVaultGeneration: 1}, keys.PersonalKey, keys.WriterSeed, values)
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(struct {
		OperationID string `json:"operation_id"`
		Envelope    string `json:"envelope"`
	}{"custody_scope", base64.RawURLEncoding.EncodeToString(scope.Raw)})
	if err != nil {
		t.Fatal(err)
	}
	record := PasswordVaultRecord{Head: head, Envelope: raw, Payload: payload, Operation: &VaultOperation{Kind: "scope-put", OwnerKind: "personal", OwnerID: account, Request: request}}
	unlock, err := store.LockEnvironmentMutations(issuer, account, "password_vault")
	if err != nil {
		t.Fatal(err)
	}
	saveErr := store.SavePasswordVault(record)
	unlockErr := unlock()
	if err := errors.Join(saveErr, unlockErr); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadPasswordVault(issuer, account)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.Payload, payload) || loaded.Operation == nil || !bytes.Equal(loaded.Operation.Request, request) {
		loaded.Clear()
		t.Fatal("native custody changed key inventory or pending ciphertext")
	}
	loaded.Clear()
	if err := store.LockPasswordVault(issuer, account); err != nil {
		t.Fatal(err)
	}
	locked, err := store.LoadPasswordVault(issuer, account)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Clear()
	if len(locked.Payload) != 0 || !bytes.Equal(locked.Envelope, raw) || locked.Operation == nil || !bytes.Equal(locked.Operation.Request, request) {
		t.Fatal("lock destroyed encrypted recovery or retained unlocked keys")
	}
	if err := store.RemovePasswordVault(issuer, account); err != nil {
		t.Fatal(err)
	}
	if after, err := store.LoadPasswordVault(issuer, account); !errors.Is(err, ErrSecretNotFound) {
		after.Clear()
		t.Fatal("explicit removal did not remove task custody", err)
	}
	t.Log("production secure store retained 128 team keys and a full-sized encrypted pending scope; lock and explicit removal passed")
}
