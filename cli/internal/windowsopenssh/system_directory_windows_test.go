//go:build windows

package windowsopenssh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// A user can set WINDIR for the process they elevate; the elevated checks must not load a module from there.
func TestSecurityModuleIgnoresAPoisonedWINDIR(t *testing.T) {
	poisoned := t.TempDir()
	modules := filepath.Join(poisoned, `System32\WindowsPowerShell\v1.0\Modules\Microsoft.PowerShell.Security`)
	if err := os.MkdirAll(modules, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"Microsoft.PowerShell.Security.psd1": "@{ModuleVersion='9.9';RootModule='evil.psm1';FunctionsToExport='*'}",
		"evil.psm1":                          "Write-Output 'POISONED'; Set-Content (Join-Path $PSScriptRoot 'ran') x",
	} {
		if err := os.WriteFile(filepath.Join(modules, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WINDIR", poisoned)
	script := securityModuleImport + "Write-Output (Get-AuthenticodeSignature -LiteralPath '" + filepath.Join(system, "notepad.exe") + "').Status"
	output, err := CommandRunner{}.Run(context.Background(), "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	if _, statErr := os.Lstat(filepath.Join(modules, "ran")); statErr == nil || strings.Contains(string(output), "POISONED") {
		t.Fatalf("a module under WINDIR was loaded: %s", boundedOutput(output))
	}
	if err != nil || !strings.Contains(string(output), "Valid") {
		t.Fatalf("signature check under a poisoned WINDIR: %v %s", err, boundedOutput(output))
	}
}
