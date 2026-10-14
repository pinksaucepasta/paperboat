//go:build windows

package hostinstall

import (
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeCarrierLockPublication(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("elevated native Windows fixture required")
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "owner.lock")
	descriptor := carrierLockDACL(user.User.Sid.String())
	if err := atomicfile.Write(path, nil, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SY" + descriptor}); err != nil {
		t.Fatal(err)
	}
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(ptr, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if !windowssecurity.HandleOwnerMatchesSID(h, system) || !windowssecurity.ProtectedHandleDACLMatches(h, descriptor) {
		t.Fatal("published lock does not have exact SYSTEM custody and enrolled read/write permissions")
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatal("lock publication changed file contents")
	}
}
