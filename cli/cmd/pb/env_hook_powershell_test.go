package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// powerShellHostile holds every character that could end or interpolate a
// PowerShell string: both quotes, $, backtick, ;, %, \, a here-string start,
// a subexpression, newlines and the four typographic single quotes.
const powerShellHostile = "it's \"q\" $HOME $(Get-Date) `n ; % \\ @'\nx'@\r\n\u2018\u2019\u201a\u201b'; $env:PB_PWNED = 'yes'; '"

func powerShellExecutable(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"pwsh", "powershell"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Skip("PowerShell not installed")
	return ""
}

// runPowerShell runs script through -EncodedCommand, which has no quoting layer.
func runPowerShell(t *testing.T, script string, env ...string) string {
	t.Helper()
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 0, len(units)*2)
	for _, u := range units {
		raw = append(raw, byte(u), byte(u>>8))
	}
	command := exec.Command(powerShellExecutable(t), "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(raw))
	command.Env = append(os.Environ(), env...)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("powershell: %v\n%s", err, out)
	}
	return string(out)
}

const powerShellEchoUTF8 = "function Show($n) { [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes([Environment]::GetEnvironmentVariable($n))) };"

func decodedPowerShellValue(t *testing.T, out string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	return string(raw)
}

func TestPowerShellAssignmentIsALiteralInTheRealShell(t *testing.T) {
	script := powerShellEchoUTF8 + "Invoke-Expression ([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" +
		base64.StdEncoding.EncodeToString([]byte(envHookAssignment(envHookPowerShell, "PB_HOOK_TEST", powerShellHostile))) +
		"'))); Show 'PB_HOOK_TEST'; if ($env:PB_PWNED) { 'PWNED' }"
	out := runPowerShell(t, script)
	if got := decodedPowerShellValue(t, out); got != powerShellHostile {
		t.Fatalf("round trip = %q, want %q", got, powerShellHostile)
	}
}

func TestPowerShellHookLineIsOneMarkedLineWithTheQuotedPath(t *testing.T) {
	line := envHookLine(envHookPowerShell, `C:\Users\O'Brien\pb.exe`)
	if strings.Contains(line, "\n") || !strings.HasSuffix(line, envHookMarker) ||
		!strings.Contains(line, `'C:\Users\O''Brien\pb.exe'`) || !strings.Contains(line, "env hook --emit powershell 2>$null") {
		t.Fatalf("line = %q", line)
	}
}

func TestPowerShellProfileInstallIsIdempotentAndUninstallKeepsTheRest(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "PowerShell", "profile.ps1")
	line := envHookLine(envHookPowerShell, `C:\Users\Zoë\pb.exe`)
	if changed, err := writeEnvHookLine(rc, line, false); err != nil || !changed {
		t.Fatalf("first install changed=%v err=%v", changed, err)
	}
	if changed, err := writeEnvHookLine(rc, line, false); err != nil || changed {
		t.Fatalf("second install changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(rc)
	if string(data) != "\ufeff"+line+"\n" {
		t.Fatalf("new profile must start with a BOM so Windows PowerShell 5.1 reads the path as UTF-8: %q", data)
	}
	own := "Set-Alias ll Get-ChildItem\n"
	if err := os.WriteFile(rc, []byte(own+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if kept, found := removeEnvHookLines(own + line + "\n"); found != 1 || kept != own {
		t.Fatalf("uninstall kept %q found %d", kept, found)
	}
}

func TestPowerShellHookNamesTheOnlySupportedShellsOnWindows(t *testing.T) {
	files := envHookRCFiles(`C:\Users\a`, envHookPowerShell)
	if len(files) != 2 || filepath.Base(filepath.Dir(files[0])) != "WindowsPowerShell" || filepath.Base(filepath.Dir(files[1])) != "PowerShell" {
		t.Fatalf("profiles = %v", files)
	}
}

// A profile written with CRLF line endings (and a BOM) keeps them: the hook line is appended with CRLF, a second install changes
// nothing, and uninstall returns the user's bytes.
func TestPowerShellProfileWithCRLFKeepsItsLineEndings(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "PowerShell", "profile.ps1")
	own := "\ufeff# mine\r\n$x = 1\r\n"
	if err := os.MkdirAll(filepath.Dir(rc), 0o755); err != nil || os.WriteFile(rc, []byte(own), 0o600) != nil {
		t.Fatal("setup")
	}
	line := envHookLine(envHookPowerShell, `C:\Users\a\pb.exe`)
	for i, wantChanged := range []bool{true, false} {
		if changed, err := writeEnvHookLine(rc, line, false); err != nil || changed != wantChanged {
			t.Fatalf("install %d changed=%v err=%v", i, changed, err)
		}
	}
	data, _ := os.ReadFile(rc)
	if string(data) != own+line+"\r\n" {
		t.Fatalf("profile = %q", data)
	}
	if kept, found := removeEnvHookLines(string(data)); found != 1 || kept != own {
		t.Fatalf("uninstall kept %q found %d", kept, found)
	}
}
