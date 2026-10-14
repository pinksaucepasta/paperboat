//go:build windows

package hostinstall

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// The public reader exercises custody, native sharing errors and fresh recovery
// without changing an installed declaration or any service registration.
func TestNativeHostInstallStateFailureAndRecovery(t *testing.T) {
	if !isAdministrator() {
		t.Skip("requires an elevated Windows token for the existing SYSTEM-owner policy")
	}
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	ownerSID := tokenUser.User.Sid.String()
	previous := windowsProgramDataRoot
	windowsProgramDataRoot = filepath.Join(t.TempDir(), "Paperboat")
	t.Cleanup(func() { windowsProgramDataRoot = previous })
	config := WindowsRuntimeConfig{Schema: windowsConfigSchema, OwnerSID: ownerSID, User: "fixture", StateRoot: filepath.Join(windowsProgramDataRoot, "state"), Workspace: filepath.Join(windowsProgramDataRoot, "workspace"), ListenAddress: "127.0.0.1:8080", EnrollmentPending: true, TokenFile: WindowsHostdTokenPath(), Source: installsource.Source{Version: "dev", Platform: "windows", Architecture: runtime.GOARCH, SHA256: strings.Repeat("0", 64), Length: 1, Distribution: installsource.Custom}}
	if _, err := LoadWindowsRuntimeConfig(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing protected state cause lost: %T", err)
	}
	if err := writeWindowsConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := ensureWindowsToken(ownerSID); err != nil {
		t.Fatal(err)
	}
	requireHealthy := func() {
		t.Helper()
		actual, err := LoadWindowsRuntimeConfig()
		if err != nil || actual.OwnerSID != ownerSID || actual.Source != config.Source {
			t.Fatalf("protected state did not recover: %T", err)
		}
	}
	requireHealthy()
	pointer, err := windows.UTF16PtrFromString(WindowsInstallConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := LoadWindowsRuntimeConfig()
	closeErr := windows.CloseHandle(handle)
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(readErr, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("native read cause lost: %T", readErr)
	}
	if strings.Contains(readErr.Error(), windowsProgramDataRoot) || strings.Contains(readErr.Error(), ownerSID) {
		t.Fatal("private state metadata exposed")
	}
	requireHealthy()
	trusted, err := windowsRuntimeTrustedOwner()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	malformed := strings.Replace(string(body), `"schema":"`+windowsConfigSchema+`"`, `"schema":123`, 1)
	if err := windowssecurity.WithRestorePrivilege(func() error { return os.WriteFile(WindowsInstallConfigPath(), []byte(malformed), 0600) }); err != nil {
		t.Fatal(err)
	}
	_, decodeErr := LoadWindowsRuntimeConfig()
	var typed *json.UnmarshalTypeError
	if !errors.As(decodeErr, &typed) || !errors.Is(decodeErr, ErrInvalidRequest) {
		t.Fatalf("protected parser cause lost: %T", decodeErr)
	}
	if err := writeWindowsConfig(config); err != nil {
		t.Fatal(err)
	}
	requireHealthy()
	// A native closed-handle probe must retain the Win32 cause, not report a
	// policy mismatch. The same validator then accepts a fresh protected object.
	object, err := openWindowsRuntimeSecurityObject(WindowsInstallConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.CloseHandle(object); err != nil {
		t.Fatal(err)
	}
	if err := checkWindowsRuntimeHandleSecurity(object, trusted, windowsRuntimeCurrentFileDACL(ownerSID)); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		t.Fatalf("native security probe cause lost: %T", err)
	}
	requireHealthy()
}
