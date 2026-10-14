//go:build windows

package process

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsShellRetainsOwnerProfileEnvironment(t *testing.T) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(system, "cmd.exe")
	owner, err := windows.GetCurrentProcessToken().KnownFolderPath(windows.FOLDERID_Profile, windows.KF_FLAG_DEFAULT)
	if err != nil {
		t.Fatal(err)
	}
	manager := &sessions{}
	launcher, err := NewShellLauncher(shell, []string{"PATH=" + system, "USERPROFILE=" + owner, "LOCALAPPDATA=" + filepath.Join(owner, "AppData", "Local"), "APPDATA=" + filepath.Join(owner, "AppData", "Roaming")}, manager)
	if err != nil {
		t.Fatalf("owner profile environment rejected: %v", err)
	}
	if _, err := launcher.Launch(context.Background(), LaunchRequest{ID: "ses_profile", CWD: owner, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}); err != nil {
		t.Fatal(err)
	}
	if environmentValue(manager.created[0].Command.Env, "USERPROFILE") != owner {
		t.Fatal("owner profile was dropped")
	}
	if _, err := launcher.Launch(context.Background(), LaunchRequest{ID: "ses_injected_profile", Environment: map[string]string{"USERPROFILE": "C:\\other-user"}}); err == nil {
		t.Fatal("client replaced owner profile")
	}
}

func TestWindowsBaseEnvironmentPreservesProfileAndExecutablePath(t *testing.T) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(system, "cmd.exe")
	t.Setenv("PAPERBOAT_HOSTD_TOKEN_FILE", "private-control-token-file")
	environment, err := BaseEnvironment(shell)
	if err != nil {
		t.Fatal(err)
	}
	for _, folder := range []struct {
		key string
		id  *windows.KNOWNFOLDERID
	}{{"USERPROFILE", windows.FOLDERID_Profile}, {"APPDATA", windows.FOLDERID_RoamingAppData}, {"LOCALAPPDATA", windows.FOLDERID_LocalAppData}} {
		expected, err := windows.GetCurrentProcessToken().KnownFolderPath(folder.id, windows.KF_FLAG_DEFAULT)
		if err != nil || environmentValue(environment, folder.key) != expected {
			t.Fatalf("owner folder %s unavailable", folder.key)
		}
	}
	if environmentValue(environment, "PAPERBOAT_HOSTD_TOKEN_FILE") != "" {
		t.Fatal("service credential reference entered application environment")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shell, "/d", "/c", "echo %USERPROFILE%&& echo %APPDATA%&& echo %LOCALAPPDATA%&& where "+filepath.Base(executable))
	cmd.Env = environment
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("native profile/executable lookup failed: %v", err)
	}
	for _, key := range []string{"USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
		if !strings.Contains(string(output), environmentValue(environment, key)) {
			t.Fatalf("child omitted %s", key)
		}
	}
	if !strings.Contains(strings.ToLower(string(output)), strings.ToLower(executable)) {
		t.Fatal("current installed executable directory missing from PATH")
	}
}
