//go:build darwin || linux

package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type keychainVaultTestSecretStore struct {
	mu         sync.Mutex
	values     map[string]string
	failDelete bool
}

func (s *keychainVaultTestSecretStore) Set(ref, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[ref] = value
	return nil
}

func (s *keychainVaultTestSecretStore) Get(ref string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[ref]
	if !ok {
		return "", ErrSecretNotFound
	}
	return value, nil
}

func (s *keychainVaultTestSecretStore) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failDelete {
		return errors.New("injected wrapping-key deletion failure")
	}
	delete(s.values, ref)
	return nil
}

func (s *keychainVaultTestSecretStore) snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := make(map[string]string, len(s.values))
	for ref, value := range s.values {
		copy[ref] = value
	}
	return copy
}

func (s *keychainVaultTestSecretStore) setValue(ref, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[ref] = value
}

func (s *keychainVaultTestSecretStore) deleteValue(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, ref)
}

func newKeychainVaultTestStore(t *testing.T) (keychainVaultStore, *keychainVaultTestSecretStore, string, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets := &keychainVaultTestSecretStore{values: make(map[string]string)}
	ref := "environment-password-vault-v1-" + strings.Repeat("a", 32)
	return keychainVaultStore{directory: directory, keys: secrets}, secrets, directory, ref
}

func keychainVaultPath(directory, ref string) string {
	return filepath.Join(directory, ref+".sealed")
}

func requireCredentialStoreUnavailable(t *testing.T, err error) {
	t.Helper()
	if err == nil || !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("error=%v, want ErrCredentialStoreUnavailable", err)
	}
}

func requireKeychainGetUnavailable(t *testing.T, store keychainVaultStore, ref string) {
	t.Helper()
	_, err := store.Get(ref)
	requireCredentialStoreUnavailable(t, err)
}

func TestKeychainVaultStoreRoundTripAtBoundAndSmallWrappingSecret(t *testing.T) {
	store, secrets, directory, ref := newKeychainVaultTestStore(t)
	value := string(bytes.Repeat([]byte{'v'}, passwordVaultRecordBytes))
	if err := store.Set(ref, value); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != value {
		t.Fatalf("round trip length=%d, want %d", len(got), len(value))
	}
	raw, err := os.ReadFile(keychainVaultPath(directory, ref))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != passwordVaultRecordBytes+1+12+16 {
		t.Fatalf("sealed length=%d, want %d", len(raw), passwordVaultRecordBytes+29)
	}
	if bytes.Contains(raw, []byte(value[:64])) {
		t.Fatal("sealed file contains plaintext")
	}
	clear(raw)
	stored := secrets.snapshot()
	if len(stored) != 1 {
		t.Fatalf("underlying secret count=%d, want one wrapping key", len(stored))
	}
	wrapping, ok := stored[ref+"-wrapping-key"]
	if !ok {
		t.Fatalf("underlying refs=%v", stored)
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(wrapping)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != wrapping {
		t.Fatalf("wrapping secret is not canonical 32-byte base64: len=%d err=%v", len(decoded), err)
	}
	clear(decoded)
	before, err := os.ReadFile(keychainVaultPath(directory, ref))
	if err != nil {
		t.Fatal(err)
	}
	oversize := string(bytes.Repeat([]byte{'x'}, passwordVaultRecordBytes+1))
	requireCredentialStoreUnavailable(t, store.Set(ref, oversize))
	after, err := os.ReadFile(keychainVaultPath(directory, ref))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("oversize Set replaced the current sealed value")
	}
	clear(before)
	clear(after)
}

func TestKeychainVaultStoreTamperSubstitutionAndMissingWrappingKeyFailClosed(t *testing.T) {
	store, secrets, directory, ref := newKeychainVaultTestStore(t)
	if err := store.Set(ref, "authenticated value"); err != nil {
		t.Fatal(err)
	}
	path := keychainVaultPath(directory, ref)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(original)
	tampered[len(tampered)-1] ^= 1
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	requireKeychainGetUnavailable(t, store, ref)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	clear(tampered)

	stored := secrets.snapshot()
	originalWrapping := stored[ref+"-wrapping-key"]
	substitute := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x19}, 32))
	secrets.setValue(ref+"-wrapping-key", substitute)
	requireKeychainGetUnavailable(t, store, ref)
	secrets.setValue(ref+"-wrapping-key", originalWrapping)
	secrets.deleteValue(ref + "-wrapping-key")
	requireKeychainGetUnavailable(t, store, ref)
	clear(original)
}

func TestKeychainVaultStoreRejectsLooseModesAndSymlinks(t *testing.T) {
	store, _, directory, ref := newKeychainVaultTestStore(t)
	if err := store.Set(ref, "protected value"); err != nil {
		t.Fatal(err)
	}
	path := keychainVaultPath(directory, ref)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	requireKeychainGetUnavailable(t, store, ref)
	requireCredentialStoreUnavailable(t, store.Set(ref, "replacement"))
	requireCredentialStoreUnavailable(t, store.Delete(ref))
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, current) {
		t.Fatal("loose-mode operations changed the sealed file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	clear(original)
	clear(current)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "untrusted-target")
	if err := os.WriteFile(target, []byte("not a sealed vault"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ref); err == nil {
		t.Fatal("Get followed a sealed-file symlink")
	}
	if err := store.Set(ref, "replacement"); err == nil {
		t.Fatal("Set accepted a sealed-file symlink")
	}
	if err := store.Delete(ref); err == nil {
		t.Fatal("Delete accepted a sealed-file symlink")
	}
	if targetValue, err := os.ReadFile(target); err != nil || string(targetValue) != "not a sealed vault" {
		t.Fatalf("symlink target changed: %q err=%v", targetValue, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	linkedParent := filepath.Join(t.TempDir(), "credentials-link")
	if err := os.Symlink(directory, linkedParent); err != nil {
		t.Fatal(err)
	}
	linkedStore := keychainVaultStore{directory: linkedParent, keys: store.keys}
	if err := linkedStore.Set(ref, "replacement"); err == nil {
		t.Fatal("Set accepted a symlinked credential directory")
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ref, "replacement"); err == nil {
		t.Fatal("Set accepted a loose credential directory")
	}
}

func TestKeychainVaultStoreDeletePartialFailureAndRecreation(t *testing.T) {
	store, secrets, directory, ref := newKeychainVaultTestStore(t)
	if err := store.Set(ref, "first value"); err != nil {
		t.Fatal(err)
	}
	secrets.failDelete = true
	if err := store.Delete(ref); err == nil {
		t.Fatal("Delete hid wrapping-key deletion failure")
	}
	secrets.failDelete = false
	if _, err := os.Stat(keychainVaultPath(directory, ref)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sealed file after partial delete: %v", err)
	}
	if err := store.Set(ref, "recovered value"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(ref); err != nil || got != "recovered value" {
		t.Fatalf("recovered value=%q err=%v", got, err)
	}
	if err := store.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keychainVaultPath(directory, ref)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sealed file after delete: %v", err)
	}
	if _, err := store.Get(ref); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing sealed value error=%v", err)
	}
	if _, ok := secrets.snapshot()[ref+"-wrapping-key"]; ok {
		t.Fatal("Delete retained the wrapping key")
	}
	if err := store.Set(ref, "new value"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(ref); err != nil || got != "new value" {
		t.Fatalf("recreated value=%q err=%v", got, err)
	}
}

func TestKeychainVaultStoreSerializesConcurrentWritersAndReaders(t *testing.T) {
	store, _, _, ref := newKeychainVaultTestStore(t)
	if err := store.Set(ref, "seed"); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	const iterations = 16
	errorsCh := make(chan error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		worker := worker
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				value := fmt.Sprintf("worker-%d-%d", worker, iteration)
				if err := store.Set(ref, value); err != nil {
					errorsCh <- err
					return
				}
				got, err := store.Get(ref)
				if err != nil {
					errorsCh <- err
					return
				}
				if got == "" || !strings.HasPrefix(got, "worker-") {
					errorsCh <- fmt.Errorf("unexpected concurrent value %q", got)
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
}

func TestKeychainVaultReferenceValidation(t *testing.T) {
	store, _, _, valid := newKeychainVaultTestStore(t)
	for _, ref := range []string{
		"", "environment-password-vault-v1-", "environment-password-vault-v1-" + strings.Repeat("A", 32),
		"environment-password-vault-v1-" + strings.Repeat("a", 31), "environment-password-vault-v1-" + strings.Repeat("g", 32),
	} {
		if keychainVaultReference(ref) {
			t.Fatalf("invalid reference accepted: %q", ref)
		}
		if err := store.Set(ref, "value"); !errors.Is(err, ErrCredentialStoreUnavailable) {
			t.Fatalf("Set(%q) error=%v", ref, err)
		}
	}
	if !keychainVaultReference(valid) {
		t.Fatalf("valid reference rejected: %q", valid)
	}
}
