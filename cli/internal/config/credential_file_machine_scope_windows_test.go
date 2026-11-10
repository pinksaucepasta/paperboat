//go:build windows

package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestWindowsFileSecretUsesMachineScopeReferenceBoundEnvelope(t *testing.T) {
	store := FileSecretStore{Dir: t.TempDir()}
	ref := fmt.Sprintf("machine-file-secret-%d", time.Now().UnixNano())
	want := "paperboat logged-out S4U transfer key"
	if err := store.Set(ref, want); err != nil {
		t.Fatal(err)
	}
	path := store.path(ref)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	header, err := windowsFileSecretV2Header(path, false)
	if err != nil || !bytes.HasPrefix(body, header) || bytes.Contains(body, []byte(want)) {
		t.Fatalf("file-secret v2 envelope header=%t plaintext=%t err=%v", bytes.HasPrefix(body, header), bytes.Contains(body, []byte(want)), err)
	}
	if got, err := store.Get(ref); err != nil || got != want {
		t.Fatalf("machine-scope round trip value=%q err=%v", got, err)
	}

	otherRef := ref + "-other"
	if err := store.Set(otherRef, "placeholder"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path(otherRef), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(otherRef); got != "" || !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("wrong-reference envelope value=%q err=%v", got, err)
	}

	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-1] ^= 0xff
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(ref); got != "" || !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("tampered v2 envelope value=%q err=%v", got, err)
	}
}

func TestWindowsFileSecretRejectsNonRegularCredentialObject(t *testing.T) {
	directory, _ := preparePrivateWindowsCredentialDirectory(t)
	store := FileSecretStore{Dir: directory}
	ref := fmt.Sprintf("directory-secret-%d", time.Now().UnixNano())
	path := store.path(ref)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	value, err := store.Get(ref)
	if value != "" || !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatal("directory credential returned a value or lost its invalid-store error")
	}
}
