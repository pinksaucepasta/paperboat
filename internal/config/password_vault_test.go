package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

func passwordVaultTestHead(issuer, accountID string, generation uint64, envelope []byte) environmente2ee.VaultHead {
	return environmente2ee.VaultHead{
		Issuer: issuer, AccountID: accountID, Generation: generation,
		ID: environmente2ee.DocumentID(sha256.Sum256(envelope)),
	}
}

func passwordVaultTestEnvelope(t *testing.T, issuer, accountID string, generation uint64, previous environmente2ee.DocumentID) ([]byte, environmente2ee.VaultHead) {
	t.Helper()
	keys, err := environmente2ee.NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Clear()
	payload, err := keys.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(payload)
	raw, head, err := environmente2ee.SealPasswordVault(context.Background(), environmente2ee.VaultHead{
		Issuer: issuer, AccountID: accountID, Generation: generation,
	}, previous, []byte("test-password"), payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw, head
}

func TestPasswordVaultCustodyRequiresSecureStore(t *testing.T) {
	issuer := "https://api.example.test"
	envelope := []byte("encrypted-vault-envelope")
	record := PasswordVaultRecord{Head: passwordVaultTestHead(issuer, "account_1", 1, envelope), Envelope: envelope}
	store := ProfileStore{Path: t.TempDir(), Secrets: FileSecretStore{Dir: t.TempDir()}}
	if err := store.SavePasswordVault(record); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("file fallback error = %v", err)
	}
}

func TestPasswordVaultCustodyRejectsAccountSubstitution(t *testing.T) {
	issuer := "https://api.example.test"
	accountID := "account_1"
	secrets := &environmentSecureMemoryStore{}
	store := ProfileStore{Path: t.TempDir(), Secrets: secrets}
	envelope, head := passwordVaultTestEnvelope(t, issuer, accountID, 1, environmente2ee.DocumentID{})
	record := PasswordVaultRecord{Head: head, Envelope: envelope, Payload: passwordVaultTestPayload(t, envelope, head)}
	if err := store.SavePasswordVault(record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadPasswordVault(issuer, "account_2"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("wrong-account load error = %v", err)
	}
	substitutedEnvelope, substitutedHead := passwordVaultTestEnvelope(t, issuer, "account_2", 1, environmente2ee.DocumentID{})
	stored, err := encodePasswordVaultRecord(PasswordVaultRecord{Head: substitutedHead, Envelope: substitutedEnvelope})
	if err != nil {
		t.Fatal(err)
	}
	secrets.values[passwordVaultSecretRef(issuer, accountID)] = string(stored)
	clear(stored)
	if _, err := store.LoadPasswordVault(issuer, accountID); !errors.Is(err, ErrPasswordVaultInvalid) {
		t.Fatalf("substituted record error = %v", err)
	}
}

func TestPasswordVaultLockRetainsEnvelopesAndClearsPayloads(t *testing.T) {
	issuer := "https://api.example.test"
	accountID := "account_1"
	secrets := &environmentSecureMemoryStore{}
	store := ProfileStore{Path: t.TempDir(), Secrets: secrets}
	currentEnvelope, currentHead := passwordVaultTestEnvelope(t, issuer, accountID, 1, environmente2ee.DocumentID{})
	pendingEnvelope, pendingHead := passwordVaultTestEnvelope(t, issuer, accountID, 2, currentHead.ID)
	record := PasswordVaultRecord{
		Head:     currentHead,
		Envelope: currentEnvelope,
		Payload:  passwordVaultTestPayload(t, currentEnvelope, currentHead),
		Pending: &PasswordVaultPending{
			Head:     pendingHead,
			Envelope: pendingEnvelope,
			Payload:  passwordVaultTestPayload(t, pendingEnvelope, pendingHead),
		},
	}
	if err := store.SavePasswordVault(record); err != nil {
		t.Fatal(err)
	}
	if err := store.LockPasswordVault(issuer, accountID); err != nil {
		t.Fatal(err)
	}
	locked, err := store.LoadPasswordVault(issuer, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Clear()
	if len(locked.Payload) != 0 || locked.Pending == nil || len(locked.Pending.Payload) != 0 {
		t.Fatal("lock retained payloads")
	}
	if locked.Head != record.Head || !bytes.Equal(locked.Envelope, record.Envelope) || locked.Pending.Head != record.Pending.Head || !bytes.Equal(locked.Pending.Envelope, record.Pending.Envelope) {
		t.Fatal("lock changed encrypted custody")
	}
}

func TestPasswordVaultPendingInitialAndSuccessorPersistence(t *testing.T) {
	issuer := "https://api.example.test"
	accountID := "account_1"
	secrets := &environmentSecureMemoryStore{}
	store := ProfileStore{Path: t.TempDir(), Secrets: secrets}
	pendingEnvelope, pendingHead := passwordVaultTestEnvelope(t, issuer, accountID, 1, environmente2ee.DocumentID{})
	record := PasswordVaultRecord{Pending: &PasswordVaultPending{
		Head:     pendingHead,
		Envelope: pendingEnvelope,
		Payload:  passwordVaultTestPayload(t, pendingEnvelope, pendingHead),
	}}
	if err := store.SavePasswordVault(record); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadPasswordVault(issuer, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if !passwordVaultHeadIsZero(loaded.Head) || len(loaded.Envelope) != 0 || loaded.Pending == nil || !bytes.Equal(loaded.Pending.Payload, record.Pending.Payload) {
		t.Fatalf("initial pending custody = %#v", loaded)
	}
	loaded.Clear()
	if err := store.SavePasswordVault(loaded); err != nil {
		t.Fatal(err)
	}
	locked, err := store.LoadPasswordVault(issuer, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Clear()
	if locked.Pending == nil || len(locked.Pending.Payload) != 0 || !bytes.Equal(locked.Pending.Envelope, pendingEnvelope) {
		t.Fatal("lock/reconciliation state did not retain pending envelope")
	}

	bad := locked
	bad.Pending = &PasswordVaultPending{Head: passwordVaultTestHead(issuer, accountID, 3, pendingEnvelope), Envelope: pendingEnvelope}
	if err := store.SavePasswordVault(bad); !errors.Is(err, ErrPasswordVaultInvalid) {
		t.Fatalf("successor skip error = %v", err)
	}
}

func TestPasswordVaultCustodySurvivesLogoutUntilExplicitRemoval(t *testing.T) {
	issuer := "https://api.example.test"
	accountID := "account_1"
	secrets := &environmentSecureMemoryStore{}
	store := ProfileStore{Path: t.TempDir(), Secrets: secrets}
	envelope, head := passwordVaultTestEnvelope(t, issuer, accountID, 1, environmente2ee.DocumentID{})
	if err := store.SavePasswordVault(PasswordVaultRecord{Head: head, Envelope: envelope}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Profile{Issuer: issuer, Account: Account{ID: accountID}, CLIClientSessionID: "cli_1"}, Credential{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TakeLogoutCredentials(issuer); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadPasswordVault(issuer, accountID); err != nil {
		t.Fatalf("logout removed password vault: %v", err)
	}
	if err := store.RemovePasswordVault(issuer, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadPasswordVault(issuer, accountID); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("explicit removal error = %v", err)
	}
}

func passwordVaultTestPayload(t *testing.T, raw []byte, head environmente2ee.VaultHead) []byte {
	t.Helper()
	payload, err := environmente2ee.OpenPasswordVault(context.Background(), head, []byte("test-password"), raw)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
