//go:build windows

package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func TestWindowsCredentialACLProbePreservesNativeCauseAndRecovers(t *testing.T) {
	directory, sddl := preparePrivateWindowsCredentialDirectory(t)
	path := filepath.Join(directory, "bounded-credential")
	value := []byte("fixture-only-account-token")
	defer clear(value)
	if err := writeCredentialFile(path, value); err != nil {
		t.Fatal("protected credential write failed")
	}
	read, err := readCredentialFile(path)
	if err != nil || !bytes.Equal(read, value) {
		clear(read)
		t.Fatal("protected credential roundtrip failed")
	}
	clear(read)
	ownerSID, err := currentUserSID()
	if err != nil {
		t.Fatal("fixture owner unavailable")
	}
	// A genuine extra principal is a policy mismatch, not a failed native probe.
	setWindowsFixtureSecurity(t, path, ownerSID, sddl+"(A;;FR;;;AU)", true)
	if matches, err := checkCredentialFilePrivate(path); matches || err != nil {
		t.Fatal("extra-principal ACL was accepted or misclassified as probe failure")
	}
	denied, err := readCredentialFile(path)
	if err == nil || denied != nil || !errors.Is(err, ErrCredentialStoreUnavailable) {
		clear(denied)
		t.Fatal("invalid ACL returned credential data")
	}
	if err := writeCredentialFile(path, value); err != nil {
		t.Fatal("protected ACL rewrite failed to recover")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal("owned credential removal failed")
	}
	owner, err := currentUserSID()
	if err != nil {
		t.Fatal("fixture SID unavailable")
	}
	probes := []func() (bool, error){
		func() (bool, error) { return checkCredentialFilePrivate(path) },
		func() (bool, error) { return windowssecurity.CheckOwnerMatchesSID(path, owner) },
		func() (bool, error) { return windowssecurity.CheckProtectedDACLMatches(path, sddl) },
	}
	for _, probe := range probes {
		matches, err := probe()
		if matches || !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			t.Fatal("disappeared credential native ACL cause lost")
		}
	}
	if credentialFilePrivate(path) || windowssecurity.OwnerMatchesSID(path, owner) || windowssecurity.ProtectedDACLMatches(path, sddl) {
		t.Fatal("boolean adapters accepted missing object")
	}
	if err := writeCredentialFile(path, value); err != nil {
		t.Fatal("protected replacement write failed")
	}
	if private, err := checkCredentialFilePrivate(path); err != nil || !private {
		t.Fatal("protected replacement ACL did not recover")
	}
	read, err = readCredentialFile(path)
	if err != nil || !bytes.Equal(read, value) {
		clear(read)
		t.Fatal("protected replacement read failed")
	}
	clear(read)
	if matches, err := windowssecurity.CheckProtectedDACLMatches(path, "invalid-native-descriptor"); matches || err == nil {
		t.Fatal("invalid expected native descriptor lost conversion failure")
	}
}

func TestWindowsCredentialFileRejectsDirectoryAndReparseWithoutEmptySuccess(t *testing.T) {
	for _, kind := range []string{"directory", "reparse"} {
		t.Run(kind, func(t *testing.T) {
			directory, _ := preparePrivateWindowsCredentialDirectory(t)
			path := filepath.Join(directory, "credential-object")
			if kind == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal("invalid directory fixture failed")
				}
			} else {
				target := filepath.Join(directory, "actual-credential")
				if err := writeCredentialFile(target, []byte("fixture-only-reparse-token")); err != nil {
					t.Fatal("protected target creation failed")
				}
				if err := os.Symlink(target, path); err != nil {
					if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
						t.Skip("native reparse fixture requires symlink privilege")
					}
					t.Fatal("owned reparse fixture creation failed")
				}
			}
			data, err := readCredentialFile(path)
			if err == nil || !errors.Is(err, ErrCredentialStoreUnavailable) || data != nil {
				clear(data)
				t.Fatal("invalid credential object returned empty success or lost policy cause")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal("owned invalid object cleanup failed")
			}
			value := []byte("fixture-only-recovered-token")
			defer clear(value)
			if err := writeCredentialFile(path, value); err != nil {
				t.Fatal("protected recovery write failed")
			}
			data, err = readCredentialFile(path)
			if err != nil || !bytes.Equal(data, value) {
				clear(data)
				t.Fatal("protected recovery roundtrip failed")
			}
			clear(data)
		})
	}
}
