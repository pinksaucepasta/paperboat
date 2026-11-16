package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserLoginRecoveryIsPrivateAndIssuerBound(t *testing.T) {
	root := t.TempDir()
	store := ProfileStore{Path: root, Secrets: FileSecretStore{Dir: filepath.Join(root, "secrets")}}
	issuer := "https://api.example.test"
	code := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	interrupted := errors.New("interrupted")
	if e := store.WithBrowserLogin(issuer, func(s *BrowserLoginState, save func() error) error {
		*s = BrowserLoginState{Version: 1, Issuer: issuer, DeviceCode: code, ApprovalURL: "https://dashboard.example.test/cli/authorize?code=ABCD-EFGH", ExpiresAt: time.Now().Add(time.Minute), Interval: 1, PreviousSessionID: "cls_old"}
		if e := save(); e != nil {
			return e
		}
		return interrupted
	}); !errors.Is(e, interrupted) {
		t.Fatal(e)
	}
	if e := store.WithBrowserLogin(issuer, func(s *BrowserLoginState, save func() error) error {
		if s.DeviceCode != code || s.PreviousSessionID != "cls_old" {
			t.Error("lost recovery")
		}
		s.Cancelled = true
		return save()
	}); e != nil {
		t.Fatal(e)
	}
	if e := store.WithBrowserLogin("https://different.example.test", func(s *BrowserLoginState, _ func() error) error {
		if s.DeviceCode != "" {
			t.Error("cross issuer recovery")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if e := store.WithBrowserLogin(issuer, func(s *BrowserLoginState, save func() error) error {
		if !s.Cancelled {
			t.Error("lost cancellation")
		}
		*s = BrowserLoginState{}
		return save()
	}); e != nil {
		t.Fatal(e)
	}
}

func TestBrowserLoginParseFailureRetainsCauseWithoutExposingStoredState(t *testing.T) {
	const issuer = "https://api.example.test"
	root := t.TempDir()
	ref := "browser-login-v1-" + profileKey(issuer+"\x00"+filepath.Clean(root))
	secrets := &injectedCredentialStore{values: map[string]string{
		ref: `{"device_code":"PRIVATE_DEVICE_CODE",`,
	}}
	store := ProfileStore{Path: root, Secrets: secrets}
	callbackCalled := false
	err := store.WithBrowserLogin(issuer, func(*BrowserLoginState, func() error) error {
		callbackCalled = true
		return nil
	})
	var syntaxErr *json.SyntaxError
	if err == nil || err.Error() != "pending login is invalid" || (!errors.As(err, &syntaxErr) && !errors.Is(err, io.ErrUnexpectedEOF)) || strings.Contains(err.Error(), "PRIVATE_DEVICE_CODE") || callbackCalled {
		t.Fatalf("parse failure=%v callbackCalled=%t", err, callbackCalled)
	}
}

// lanex2-m 2026-11-16: a second sign-in while another process held this profile's login lock failed as a bare
// local_io error. It must say another login is running.
func TestSecondBrowserLoginNamesTheLoginInProgress(t *testing.T) {
	root := t.TempDir()
	store := ProfileStore{Path: root, Secrets: FileSecretStore{Dir: filepath.Join(root, "secrets")}}
	issuer, err := NormalizeIssuer("https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureProfileDirectory(filepath.Dir(store.profilePath(issuer))); err != nil {
		t.Fatal(err)
	}
	// Another live holder of the lock directory (no in-process mutex, as a second pb process).
	other := &sharedLock{path: store.profilePath(issuer) + ".browser-login.lock.d"}
	if err := other.Lock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Unlock() })
	old := sharedLockWait
	t.Cleanup(func() { sharedLockWait = old })
	sharedLockWait = 100 * time.Millisecond
	err = store.WithBrowserLogin(issuer, func(*BrowserLoginState, func() error) error {
		t.Error("second login ran while another held the lock")
		return nil
	})
	if !errors.Is(err, ErrLoginInProgress) {
		t.Fatalf("second login = %v, want ErrLoginInProgress", err)
	}
}
