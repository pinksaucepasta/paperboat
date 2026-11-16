//go:build windows

package process

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
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
	// `where` prints long names; the test binary's own path can be an 8.3 spelling of the same directory.
	longExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(string(output)), strings.ToLower(longExecutable)) {
		t.Fatalf("current installed executable directory missing from PATH: executable=%q output=%q", executable, output)
	}
}

// A terminal or exec session gets the environment Windows gives a logged-on user, not a short allowlist:
// `echo %OS%`, ProgramFiles, SystemDrive and COMPUTERNAME must resolve in scripts and tools.
func TestWindowsBaseEnvironmentHasStandardVariables(t *testing.T) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAPERBOAT_HOSTD_TOKEN_FILE", "private-control-token-file")
	environment, err := BaseEnvironment(filepath.Join(system, "cmd.exe"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"OS", "SystemDrive", "SystemRoot", "windir", "ComSpec", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432",
		"CommonProgramFiles", "ProgramData", "COMPUTERNAME", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "USERNAME", "TEMP", "PATHEXT", "Path"} {
		if environmentValueFold(environment, key) == "" {
			t.Errorf("standard variable %s missing from the session environment", key)
		}
	}
	for _, entry := range environment {
		if strings.HasPrefix(strings.ToUpper(entry), "PAPERBOAT_") {
			t.Errorf("hostd variable reached the session: %s", strings.SplitN(entry, "=", 2)[0])
		}
	}
	// The terminal launch merges managed ENV into the base; it must accept every name Windows hands out.
	if _, err := envinject.Merge(environment, nil); err != nil {
		t.Fatalf("managed ENV merge rejected the owner environment: %v", err)
	}
	manager := &sessions{}
	launcher, err := NewShellLauncher(filepath.Join(system, "cmd.exe"), environment, manager)
	if err != nil {
		t.Fatalf("full environment rejected by the launcher: %v", err)
	}
	if _, err := launcher.Launch(context.Background(), LaunchRequest{ID: "ses_env", Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(system, "cmd.exe"), "/d", "/c", "echo %OS%|%ProgramFiles(x86)%|%SystemDrive%|%COMPUTERNAME%")
	cmd.Env = environment
	output, err := cmd.Output()
	if err != nil || strings.Contains(string(output), "%") {
		t.Fatalf("variables did not expand: %q %v", output, err)
	}
}

func environmentValueFold(environment []string, key string) string {
	for _, entry := range environment {
		if name, value, ok := strings.Cut(entry, "="); ok && strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}
