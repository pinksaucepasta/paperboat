//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"golang.org/x/sys/windows"
)

// This opt-in qualification uses the actual installed executable, protected
// owner metadata, elevation bridge, SCM entry and enrolled-owner worker.
func TestInstalledWindowsConfigWorkerActivation(t *testing.T) {
	if os.Getenv("PAPERBOAT_TEST_INSTALLED_CONFIG_WORKER") != "1" {
		t.Skip("requires task-owned installed Windows worker qualification")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := hostinstall.WindowsInstanceForSID(user.User.Sid.String())
	if err != nil {
		t.Fatal(err)
	}
	install, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		t.Fatal(err)
	}
	if got := windowsConfigServiceStatus(); got != "not_installed" {
		t.Fatalf("qualification requires absent optional config worker, got %s", got)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), elevation.RuntimeActivationDuration)
		defer cancel()
		if _, err := manageWindowsConfigService(ctx, install.StateRoot, false); err != nil {
			t.Errorf("remove task-owned config worker: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), elevation.RuntimeActivationDuration)
	defer cancel()
	handled, err := manageWindowsConfigService(ctx, install.StateRoot, true)
	if err != nil || !handled {
		t.Fatalf("enable current owner config worker: handled=%v err=%v", handled, err)
	}
	for windowsConfigServiceStatus() != "active" {
		select {
		case <-ctx.Done():
			t.Fatalf("config worker did not become active: %s", windowsConfigServiceStatus())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Log("current enrolled-owner instance config worker active")
	// SCM can report Running before a short-lived worker exits. Verify the
	// actual enrolled-owner child across the asynchronous startup boundary.
	time.Sleep(3 * time.Second)
	probe := `$ErrorActionPreference='Stop'; $owner='` + install.OwnerSID + `'; $found=@(Get-CimInstance Win32_Process -Filter "Name = 'pb.exe'" | Where-Object { $_.CommandLine -match '__runtime-config' } | Where-Object { (Invoke-CimMethod -InputObject $_ -MethodName GetOwnerSid).Sid -eq $owner }); if($found.Count -ne 1){exit 2}; 'owner_worker_alive'`
	output, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-Command", probe).CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "owner_worker_alive" {
		t.Fatal("configuration worker did not remain alive as the enrolled owner")
	}
	if windowsConfigServiceStatus() != "active" {
		t.Fatal("configuration bridge stopped after asynchronous startup")
	}

	if _, err := manageWindowsConfigService(ctx, install.StateRoot, false); err != nil {
		t.Fatal(err)
	}
	if got := windowsConfigServiceStatus(); got != "not_installed" {
		t.Fatalf("worker removal did not converge: %s", got)
	}
	t.Log("current enrolled-owner instance config worker removed")
}
