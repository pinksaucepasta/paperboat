package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"path/filepath"
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
