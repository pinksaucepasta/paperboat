//go:build windows

package service

import (
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeWindowsDeclarationPublishedWithSystemOwner(t *testing.T) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if !token.IsElevated() || user.User.Sid.String() == "S-1-5-18" {
		t.Skip("elevated enrolled administrator fixture required")
	}
	root := t.TempDir()
	if err := prepareAtomicDirectory(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "owned.json")
	body := []byte("task-owned native declaration")
	if err := writeServiceDefinition(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if !windowssecurity.OwnerMatchesSID(path, system) || !windowssecurity.ProtectedDACLMatches(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)") {
		t.Fatal("published declaration lacks canonical SYSTEM custody")
	}
	found, err := os.ReadFile(path)
	if err != nil || string(found) != string(body) {
		t.Fatal("declaration bytes changed", err)
	}
}
