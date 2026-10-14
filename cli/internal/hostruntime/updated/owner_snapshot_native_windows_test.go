//go:build windows

package updated

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

// Exercise the actual protected enrolled installation and its real owner pipe.

func TestNativeWindowsPrepareInstallRejectsRunningUpdaterWithoutHandleLeak(t *testing.T) {
	instance := os.Getenv("PAPERBOAT_NATIVE_OWNER_INSTANCE")
	if instance == "" {
		t.Skip("requires enrolled native Windows fixture")
	}
	token, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal("cannot identify test token")
	}
	if token.User.Sid.String() != "S-1-5-18" {
		t.Skip("requires SYSTEM token")
	}
	config, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		t.Fatal("protected owner config unavailable")
	}
	layout, err := service.WindowsUserLayout(config.OwnerSID)
	if err != nil || layout.Instance != instance {
		t.Fatal("owner layout mismatch")
	}
	hostdName, updaterName, err := windowsInstanceServiceNames(config.OwnerSID)
	if err != nil {
		t.Fatal("owner service identity unavailable")
	}
	if err := requireNamedWindowsServicesRunning(hostdName, updaterName); err != nil {
		t.Fatal("enrolled owner services are not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	checkUnavailable := func() {
		t.Helper()
		identity, restore, err := PrepareWindowsNativeInstall(ctx, config.OwnerSID)
		if !errors.Is(err, ErrWindowsActivationUnavailable) {
			t.Fatalf("running updater was not rejected with the expected status: %v", err)
		}
		if identity != nil || restore != nil {
			t.Fatal("rejected native install returned state or a restore callback")
		}
	}
	checkUnavailable() // Warm up runtime and SCM paths before measuring handles.
	baseline := nativeWindowsProcessHandleCount(t)
	for range 20 {
		checkUnavailable()
	}
	if err := requireNamedWindowsServicesRunning(hostdName, updaterName); err != nil {
		t.Fatal("native install rejection changed owner service state")
	}
	if err := config.Source.Verify(layout.Binary); err != nil {
		t.Fatal("native install rejection changed the installed binary")
	}
	after := nativeWindowsProcessHandleCount(t)
	t.Logf("running-updater rejection process handles: baseline=%d after20=%d", baseline, after)
	if after > baseline+4 {
		t.Fatalf("repeated native install checks leaked process handles: baseline=%d after20=%d", baseline, after)
	}
}

func nativeWindowsProcessHandleCount(t *testing.T) uint32 {
	t.Helper()
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")
	var count uint32
	result, _, callErr := proc.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&count)))
	runtime.KeepAlive(&count)
	if result == 0 {
		t.Fatalf("GetProcessHandleCount failed: %v", callErr)
	}
	return count
}
