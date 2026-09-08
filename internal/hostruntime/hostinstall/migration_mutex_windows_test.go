//go:build windows

package hostinstall

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

func TestMigrationMutexNativeElevatedOwner(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	name := fmt.Sprintf(`Global\PaperboatTask23MigrationMutex-%d`, os.Getpid())
	unlock, err := lockWindowsLocalDaemonMigrationNamed(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	// Reopening the exact name must retain the same ownership/access boundary.
	again, err := lockWindowsLocalDaemonMigrationNamed(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	again()
	n, _ := windows.UTF16PtrFromString(name)
	h, err := windows.OpenMutex(windows.READ_CONTROL, false, n)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	// The kernel maps GENERIC_ALL to MUTEX_ALL_ACCESS (STANDARD_RIGHTS_REQUIRED,
	// SYNCHRONIZE, and MUTEX_MODIFY_STATE) when creating this object.
	want, _ := windows.SecurityDescriptorFromString(`D:P(A;;0x1f0001;;;SY)(A;;0x1f0001;;;BA)`)
	if sd.String() != want.String() {
		t.Fatalf("unexpected mutex access: %s", sd.String())
	}
}
