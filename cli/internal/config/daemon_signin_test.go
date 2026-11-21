package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// lockedKeychain answers like a locked macOS login keychain.
type lockedKeychain struct{ FileSecretStore }

func (lockedKeychain) Get(string) (string, error) {
	return "", fmt.Errorf("login keychain locked: %w", ErrCredentialStoreUnavailable)
}

func TestCheckSignInNamesLockedKeychain(t *testing.T) {
	const issuer = "https://api.example.test"
	dir := t.TempDir()
	files := FileSecretStore{Dir: filepath.Join(dir, "secrets")}
	store := ProfileStore{Path: dir, Secrets: files}
	if err := checkSignIn(store, issuer); err != nil {
		t.Fatalf("signed out: %v", err)
	}
	if err := store.Save(Profile{Issuer: issuer, CLIClientSessionID: "cls_1"}, Credential{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := checkSignIn(store, issuer); err != nil {
		t.Fatalf("unlocked: %v", err)
	}
	store.Secrets = lockedKeychain{files}
	if err := checkSignIn(store, issuer); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("locked keychain: %v", err)
	}
}

// A locked keychain answers at once instead of a `security` child waiting on the unlock dialog.
func TestBoundedKeychainFailsAtOnceWhenLocked(t *testing.T) {
	defer func(previous func() bool) { keychainLocked = previous }(keychainLocked)
	defer func(tty func() bool) { stdinIsTerminal = tty }(stdinIsTerminal)
	keychainLocked = func() bool { return true }
	stdinIsTerminal = func() bool { return false }
	started := time.Now()
	_, err := boundedKeychain(func() (string, error) { time.Sleep(time.Hour); return "", nil })
	if !errors.Is(err, ErrCredentialStoreUnavailable) || time.Since(started) > time.Second {
		t.Fatalf("locked keychain: %v after %s", err, time.Since(started))
	}
}

// switchableKeychain is locked while *locked is true.
type switchableKeychain struct {
	FileSecretStore
	locked *bool
}

func (k switchableKeychain) Get(ref string) (string, error) {
	if *k.locked {
		return "", fmt.Errorf("login keychain locked: %w", ErrCredentialStoreUnavailable)
	}
	return k.FileSecretStore.Get(ref)
}

func TestCheckSignInUnlocksAtTerminalOtherwiseNamesCommand(t *testing.T) {
	const issuer = "https://api.example.test"
	dir := t.TempDir()
	locked := false
	store := ProfileStore{Path: dir, Secrets: switchableKeychain{FileSecretStore{Dir: filepath.Join(dir, "secrets")}, &locked}}
	if err := store.Save(Profile{Issuer: issuer, CLIClientSessionID: "cls_1"}, Credential{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	defer func(tty func() bool, unlock func() error) { stdinIsTerminal, unlockKeychain = tty, unlock }(stdinIsTerminal, unlockKeychain)
	unlocks := 0
	unlockKeychain = func() error { unlocks++; locked = false; return nil }

	locked = true
	stdinIsTerminal = func() bool { return false }
	if err := checkSignInUnlocking(store, issuer); !errors.Is(err, ErrCredentialStoreUnavailable) || unlocks != 0 {
		t.Fatalf("no terminal: %v, unlocks %d", err, unlocks)
	}
	stdinIsTerminal = func() bool { return true }
	if err := checkSignInUnlocking(store, issuer); err != nil || unlocks != 1 {
		t.Fatalf("terminal: %v, unlocks %d", err, unlocks)
	}
	locked = true
	unlockKeychain = func() error { unlocks++; return errors.New("exit status 51") } // wrong password
	if err := checkSignInUnlocking(store, issuer); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("failed unlock: %v", err)
	}
}

// At a terminal the unlock dialog is how the user unlocks, so the call still runs.
func TestBoundedKeychainKeepsPromptAtTerminal(t *testing.T) {
	defer func(locked, tty func() bool) { keychainLocked, stdinIsTerminal = locked, tty }(keychainLocked, stdinIsTerminal)
	keychainLocked, stdinIsTerminal = func() bool { return true }, func() bool { return true }
	if value, err := boundedKeychain(func() (string, error) { return "ok", nil }); err != nil || value != "ok" {
		t.Fatalf("terminal: %q, %v", value, err)
	}
}
