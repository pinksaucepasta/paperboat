package config

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
)

type peerTestSecretStore struct {
	values          map[string]string
	setCount        int
	failSetAt       int
	failSetErr      error
	failDeleteRef   string
	failDeleteError error
}

func (s *peerTestSecretStore) Set(ref, value string) error {
	s.setCount++
	if s.failSetAt > 0 && s.setCount == s.failSetAt {
		if s.failSetErr != nil {
			return s.failSetErr
		}
		return errors.New("injected peer identity write failure")
	}
	s.values[ref] = value
	return nil
}
func (s *peerTestSecretStore) Get(ref string) (string, error) {
	value, ok := s.values[ref]
	if !ok {
		return "", ErrSecretNotFound
	}
	return value, nil
}
func (s *peerTestSecretStore) Delete(ref string) error {
	if ref == s.failDeleteRef {
		return s.failDeleteError
	}
	delete(s.values, ref)
	return nil
}

func TestPeerIdentityKeysCreateAndReplaySeparateRecords(t *testing.T) {
	root := t.TempDir()
	secrets := &peerTestSecretStore{values: map[string]string{}}
	store := ProfileStore{Path: root, Secrets: secrets}
	first, err := store.PeerIdentityKeys("https://api.example.test", "account_1", "cli_1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.PeerIdentityKeys("https://api.example.test", "account_1", "cli_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.RootPrivate) != ed25519.PrivateKeySize || len(first.QUICPrivate) != ed25519.PrivateKeySize || !bytes.Equal(first.RootPrivate, second.RootPrivate) || !bytes.Equal(first.QUICPrivate, second.QUICPrivate) {
		t.Fatalf("identity replay mismatch")
	}
	if len(secrets.values) != 2 {
		t.Fatalf("stored records=%d", len(secrets.values))
	}
	for _, value := range secrets.values {
		if bytes.Contains([]byte(value), first.RootPrivate) || bytes.Contains([]byte(value), first.QUICPrivate) {
			t.Fatal("raw private key stored without typed encoding")
		}
	}
}

func TestPeerIdentityKeysRejectsInvalidEndpointCustody(t *testing.T) {
	root := t.TempDir()
	secrets := &peerTestSecretStore{values: map[string]string{}}
	store := ProfileStore{Path: root, Secrets: secrets}
	issuer, _ := NormalizeIssuer("https://api.example.test")
	if err := secrets.Set(peerIdentitySecretRef(issuer, "cli_1", "endpoint-quic"), "invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PeerIdentityKeys(issuer, "account_1", "cli_1"); err == nil {
		t.Fatal("invalid endpoint identity accepted")
	}
}

func TestPeerIdentityKeysRollsBackFailedCreation(t *testing.T) {
	root := t.TempDir()
	secrets := &peerTestSecretStore{values: map[string]string{}, failSetAt: 2}
	store := ProfileStore{Path: root, Secrets: secrets}
	identity, err := store.PeerIdentityKeys("https://api.example.test", "account_1", "cli_1")
	if err == nil || identity.RootPrivate != nil || identity.QUICPrivate != nil || len(secrets.values) != 0 {
		t.Fatalf("identity=%+v values=%d err=%v", identity, len(secrets.values), err)
	}
}

func TestPeerIdentityCreationPreservesRollbackFailureAndRetries(t *testing.T) {
	const issuer, accountID, endpointID = "https://api.example.test", "account_1", "cli_1"
	writeFailure := errors.New("write failed")
	cleanupFailure := errors.New("cleanup failed")
	rootRef := peerIdentitySecretRef(issuer, accountID, "account-root")
	secrets := &peerTestSecretStore{
		values:          map[string]string{},
		failSetAt:       2,
		failSetErr:      writeFailure,
		failDeleteRef:   rootRef,
		failDeleteError: cleanupFailure,
	}
	store := ProfileStore{Path: t.TempDir(), Secrets: secrets}
	identity, err := store.PeerIdentityKeys(issuer, accountID, endpointID)
	if err == nil || !errors.Is(err, writeFailure) || !errors.Is(err, cleanupFailure) || identity.RootPrivate != nil || identity.QUICPrivate != nil {
		t.Fatal("partial identity returned key material or lost write/cleanup causes")
	}
	if _, ok := secrets.values[rootRef]; !ok {
		t.Fatal("fixture did not retain the key whose rollback failed")
	}

	secrets.failDeleteRef = ""
	identity, err = store.PeerIdentityKeys(issuer, accountID, endpointID)
	if err != nil || len(identity.RootPrivate) != ed25519.PrivateKeySize || len(identity.QUICPrivate) != ed25519.PrivateKeySize {
		t.Fatal("retry did not recover a complete identity")
	}
	clearPeerIdentity(&identity)
}

func TestPeerIdentityKeysUsesProfileScopedLock(t *testing.T) {
	root := t.TempDir()
	store := ProfileStore{Path: root, Secrets: FileSecretStore{Dir: filepath.Join(root, "secrets")}}
	if _, err := store.PeerIdentityKeys("https://api.example.test", "bad\naccount", "cli_1"); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestPeerCertificateStatePersistsExactReplayAndRejectsReplacement(t *testing.T) {
	root := t.TempDir()
	secrets := &peerTestSecretStore{values: map[string]string{}}
	store := ProfileStore{Path: root, Secrets: secrets}
	raw := bytes.Repeat([]byte{7}, 172)
	first, err := store.SavePeerCertificate("https://api.example.test", "cli_1", raw)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadPeerCertificate("https://api.example.test", "cli_1")
	if err != nil || !bytes.Equal(first.Raw, loaded.Raw) {
		t.Fatalf("loaded=%x err=%v", loaded.Raw, err)
	}
	if _, err := store.SavePeerCertificate("https://api.example.test", "cli_1", bytes.Repeat([]byte{8}, 172)); err == nil {
		t.Fatal("certificate replacement accepted outside rotation")
	}
}

func TestPeerIdentityKeysForExistingRootNeverCreatesRoot(t *testing.T) {
	root := t.TempDir()
	secrets := &peerTestSecretStore{values: map[string]string{}}
	store := ProfileStore{Path: root, Secrets: secrets}
	if _, err := store.PeerIdentityKeysForExistingRoot("https://api.example.test", "account_1", "cli_1"); !errors.Is(err, ErrSecretNotFound) || len(secrets.values) != 0 {
		t.Fatalf("values=%d err=%v", len(secrets.values), err)
	}
}

func TestFreshPeerIdentityKeysUsesEndpointScopedSignerAndReplays(t *testing.T) {
	root := t.TempDir()
	secrets := &peerTestSecretStore{values: map[string]string{}}
	store := ProfileStore{Path: root, Secrets: secrets}
	issuer := "https://api.example.test"
	accountID := "account_1"
	old, err := store.PeerIdentityKeys(issuer, accountID, "cli_old")
	if err != nil {
		t.Fatal(err)
	}
	defer clearPeerIdentity(&old)
	oldRoot := append([]byte(nil), old.RootPrivate...)
	defer clear(oldRoot)

	first, err := store.FreshPeerIdentityKeys(issuer, accountID, "cli_fresh")
	if err != nil {
		t.Fatal(err)
	}
	defer clearPeerIdentity(&first)
	second, err := store.FreshPeerIdentityKeys(issuer, accountID, "cli_fresh")
	if err != nil {
		t.Fatal(err)
	}
	defer clearPeerIdentity(&second)
	if bytes.Equal(first.RootPrivate, oldRoot) {
		t.Fatal("fresh endpoint reused account-scoped signing key")
	}
	if !bytes.Equal(first.RootPrivate, second.RootPrivate) || !bytes.Equal(first.QUICPrivate, second.QUICPrivate) {
		t.Fatal("same fresh endpoint did not replay its durable identity")
	}

	other, err := store.FreshPeerIdentityKeys(issuer, accountID, "cli_other")
	if err != nil {
		t.Fatal(err)
	}
	defer clearPeerIdentity(&other)
	if bytes.Equal(first.RootPrivate, other.RootPrivate) {
		t.Fatal("different endpoint sessions share signing key")
	}
	seed, found, err := loadPeerKey(secrets, peerIdentitySecretRef(issuer, "cli_fresh", "endpoint-signing"), "endpoint_signing_seed")
	if err != nil || !found {
		t.Fatalf("fresh signing seed was not durable: found=%t err=%v", found, err)
	}
	clear(seed)
}

func TestFreshPeerIdentityKeysRejectsPartialIdentity(t *testing.T) {
	root := t.TempDir()
	secrets := &peerTestSecretStore{values: map[string]string{}}
	store := ProfileStore{Path: root, Secrets: secrets}
	issuer := "https://api.example.test"
	if err := storePeerKey(secrets, peerIdentitySecretRef(issuer, "cli_fresh", "endpoint-signing"), "endpoint_signing_seed", bytes.Repeat([]byte{3}, ed25519.SeedSize)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FreshPeerIdentityKeys(issuer, "account_1", "cli_fresh"); err == nil {
		t.Fatal("partial fresh endpoint identity accepted")
	}
}

func TestMachineSignerDoesNotReplaceENVRootVerifier(t *testing.T) {
	store := ProfileStore{Path: t.TempDir(), Secrets: &peerTestSecretStore{values: map[string]string{}}}
	issuer, accountID := "https://api.example.test", "account_1"
	envPublic, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	machinePublic, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePeerAccountRootPublic(issuer, accountID, envPublic); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePeerMachineSigningPublic(issuer, accountID, machinePublic); err != nil {
		t.Fatal(err)
	}
	gotENV, err := store.LoadPeerAccountRootPublic(issuer, accountID)
	if err != nil || !bytes.Equal(gotENV, envPublic) {
		t.Fatalf("ENV verifier changed: %v", err)
	}
	gotMachine, err := store.LoadPeerMachineSigningPublic(issuer, accountID)
	if err != nil || !bytes.Equal(gotMachine, machinePublic) {
		t.Fatalf("machine verifier changed: %v", err)
	}
}

func TestPeerAccountRootExportImportIsConflictSafe(t *testing.T) {
	issuer := "https://api.example.test"
	accountID := "account_1"
	source := ProfileStore{Path: t.TempDir(), Secrets: &peerTestSecretStore{values: map[string]string{}}}
	identity, err := source.PeerIdentityKeys(issuer, accountID, "cli_1")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := source.ExportPeerAccountRootSeed(issuer, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(seed)
	if !bytes.Equal(ed25519.NewKeyFromSeed(seed), identity.RootPrivate) {
		t.Fatal("exported root seed differs")
	}
	target := ProfileStore{Path: t.TempDir(), Secrets: &peerTestSecretStore{values: map[string]string{}}}
	if err := target.ImportPeerAccountRootSeed(issuer, accountID, seed); err != nil {
		t.Fatal(err)
	}
	if err := target.ImportPeerAccountRootSeed(issuer, accountID, seed); err != nil {
		t.Fatalf("idempotent import: %v", err)
	}
	conflict := append([]byte(nil), seed...)
	conflict[0] ^= 1
	defer clear(conflict)
	if err := target.ImportPeerAccountRootSeed(issuer, accountID, conflict); err == nil {
		t.Fatal("conflicting root import accepted")
	}
}

func TestProfileRemovalErasesPeerIdentityCustody(t *testing.T) {
	root := t.TempDir()
	secrets := &peerTestSecretStore{values: map[string]string{}}
	store := ProfileStore{Path: root, Secrets: secrets}
	issuer := "https://api.example.test"
	if _, err := store.PeerIdentityKeys(issuer, "account_1", "cli_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FreshPeerIdentityKeys(issuer, "account_1", "cli_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePeerCertificate(issuer, "cli_1", bytes.Repeat([]byte{7}, 172)); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Profile{Issuer: issuer, Account: Account{ID: "account_1"}, CLIClientSessionID: "cli_1"}, Credential{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remove(issuer); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{peerIdentitySecretRef(issuer, "account_1", "account-root"), peerIdentitySecretRef(issuer, "cli_1", "endpoint-quic"), peerIdentitySecretRef(issuer, "cli_1", "endpoint-signing"), peerIdentitySecretRef(issuer, "cli_1", "endpoint-certificate")} {
		if _, err := secrets.Get(ref); !errors.Is(err, ErrSecretNotFound) {
			t.Fatalf("secret %s remains: %v", ref, err)
		}
	}
}
