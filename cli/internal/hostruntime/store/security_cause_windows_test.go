//go:build windows

package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func TestStoreWindowsCustodyCausesAndProtectedRecovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "PRIVATE_STORE")
	state, err := Open(t.Context(), Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.CreateSession(t.Context(), testSession()); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil {
		t.Fatal("current fixture user unavailable")
	}
	for _, object := range []struct{ path, flags string }{{root, "OICI"}, {filepath.Join(root, "state.db"), ""}} {
		expected := "D:P(A;" + object.flags + ";FA;;;SY)(A;" + object.flags + ";FA;;;BA)(A;" + object.flags + ";FA;;;" + user.User.Sid.String() + ")"
		if match, err := windowssecurity.CheckProtectedDACLMatches(object.path, expected); err != nil || !match {
			t.Fatal("store protected DACL policy changed")
		}
	}
	state, err = Open(t.Context(), Config{Root: root})
	if err != nil {
		t.Fatal("protected store failed to reopen")
	}
	if sessions, err := state.Sessions(t.Context()); err != nil || len(sessions) != 1 {
		state.Close()
		t.Fatal("protected state did not survive")
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "PRIVATE_MISSING")
	err = secureStoreFile(missing)
	var pathCause *os.PathError
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || !errors.As(err, &pathCause) || errors.Is(err, ErrCorrupt) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("native missing probe lost cause/privacy or claimed corruption")
	}
	if err := os.WriteFile(missing, []byte("bounded"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := secureStoreFile(missing); err != nil {
		t.Fatal("fresh protected-file recovery failed")
	}
	if err := secureStoreFile(root); !errors.Is(err, ErrCorrupt) {
		t.Fatal("directory accepted as store file")
	}
	if err := secureStoreDirectory(filepath.Join(root, "state.db")); !errors.Is(err, ErrCorrupt) {
		t.Fatal("file accepted as store directory")
	}
	if err := secureStoreFile("PRIVATE_INVALID\x00PATH"); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("invalid native path exposed or accepted")
	}
}
