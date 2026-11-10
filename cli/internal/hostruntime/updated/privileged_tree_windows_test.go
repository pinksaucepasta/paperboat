//go:build windows

package updated

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseeligibility"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// Clean Windows Sandbox, official build: `pb update check` failed with "release deferral is unavailable: release
// eligibility directory could not be inspected". The elevated installer had created the updater's state directory
// (owner Administrators) before the service first ran; the service fixed its ACL but not its owner, and the
// eligibility store accepts only a directory LocalSystem owns.
func TestUpdaterStateIsOwnedByLocalSystemWhoeverCreatedIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "updated") // created by this test's account, as the installer creates it
	if err := os.MkdirAll(filepath.Join(root, "tuf", "index"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tuf", "index", "root.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyWindowsPrivilegedACL(root, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"); err == nil {
		t.Fatal("a directory this account just created passed as secured")
	}
	if err := secureWindowsPrivilegedTree(root); errors.Is(err, windows.ERROR_INVALID_OWNER) {
		// The service is LocalSystem and may always name itself owner. An administrator may only where its restore
		// privilege is honoured for this call (victus over SSH: yes; a hosted CI runner: no).
		t.Skipf("this account may not make LocalSystem the owner; run as LocalSystem: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, filepath.Join(root, "tuf"), filepath.Join(root, "tuf", "index", "root.json")} {
		if owned, err := windowssecurity.CheckOwnerMatchesSID(path, system); err != nil || !owned {
			t.Fatalf("%s: owned by LocalSystem = %v, %v", path, owned, err)
		}
	}
	store, err := releaseeligibility.NewFileStore(filepath.Join(root, "deferral.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, present, err := store.CurrentDeferral(context.Background()); err != nil || present {
		t.Fatalf("the eligibility store refused the secured directory: present=%v err=%v", present, err)
	}
}
