//go:build windows

package privateproxyconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Query-only qualification never installs, removes or broadcasts real proxy
// settings. SSH may be headless; that must remain an unsupported capability.
func TestWindowsInteractiveUserReadOnlyCurrentToken(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		t.Fatal("current-process token query failed")
	}
	var session uint32
	result, _, callErr := processIDToSessionID.Call(uintptr(os.Getpid()), uintptr(unsafe.Pointer(&session)))
	if result == 0 {
		t.Fatalf("session query failed: %T", callErr)
	}
	active, _, _ := wtsGetActiveConsoleSessionID.Call()
	expected := session != 0 && session == uint32(active) && uint32(active) != ^uint32(0) && !user.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
	interactive, err := (currentUserRegistry{}).InteractiveUser(t.Context())
	if err != nil || interactive != expected {
		t.Fatal("interactive-session policy/query failed")
	}
	if !interactive {
		t.Log("headless session rejected; current-process pseudo-token query succeeded")
	}
	_, err = NewWindowsAdapter(currentUserRegistry{}).Snapshot(t.Context())
	if !interactive {
		if !UnsupportedOnly(err) {
			t.Fatal("headless snapshot did not preserve unsupported ownership")
		}
	} else if err != nil {
		t.Fatal("interactive snapshot read failed")
	}
	// The direct backend query exercises real key acquisition and owned Close.
	// Some Windows profiles do not yet have this key; retain that native cause.
	for range 2 {
		_, err := (currentUserRegistry{}).GetAutoConfigURL(t.Context())
		if err != nil && !errors.Is(err, registry.ErrNotExist) {
			t.Fatalf("registry read failed: %T", err)
		}
	}
}

func TestWindowsManagerRejectsUnprotectedJournalAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	adapter := &fakeAdapter{state: "prior"}
	manager, _ := New(path, adapter)
	if err := manager.Install(t.Context(), "http://127.0.0.1:99/private.pac"); err != nil {
		t.Fatal(err)
	}
	original, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal("fixture ACL read failed")
	}
	originalDACL, _, err := original.DACL()
	if err != nil {
		t.Fatal("fixture DACL read failed")
	}
	permissive, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal("fixture descriptor failed")
	}
	dacl, _, err := permissive.DACL()
	if err != nil {
		t.Fatal("fixture descriptor DACL failed")
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal("fixture ACL change failed")
	}
	if err := manager.Recover(t.Context()); err == nil {
		t.Fatal("untrusted journal accepted")
	}
	if adapter.state != "http://127.0.0.1:99/private.pac" {
		t.Fatal("untrusted journal mutated platform")
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, originalDACL, nil); err != nil {
		t.Fatal("fixture ACL restoration failed")
	}
	if err := manager.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if adapter.state != "prior" {
		t.Fatal("protected journal recovery lost prior state")
	}
}
