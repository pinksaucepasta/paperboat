package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// renderWindowsInstaller renders install.ps1 from signed-looking metadata that pins fake. Every product pins the same bytes,
// so the architecture PowerShell reports on this machine does not matter.
func renderWindowsInstaller(t *testing.T, fake []byte) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	sum := sha256.Sum256(fake)
	digest := hex.EncodeToString(sum[:])
	const version = "2026.11.09.1"
	targets := map[string]any{}
	for _, product := range []struct{ name, platform, arch, format string }{
		{"pb-linux-amd64", "linux", "amd64", "elf"}, {"pb-linux-arm64", "linux", "arm64", "elf"}, {"pb-darwin-arm64", "darwin", "arm64", "macho"},
		{"pb-windows-amd64.exe", "windows", "amd64", "pe"}, {"pb-windows-arm64.exe", "windows", "arm64", "pe"},
	} {
		custom := map[string]any{"schema": "paperboat.tuf-asset/v1", "kind": "github-release-asset", "version": version, "platform": product.platform, "architecture": product.arch, "format": product.format, "asset_name": product.name, "repository": "example/paperboat", "url": "https://github.com/example/paperboat/releases/download/" + version + "/" + product.name, "sha256": digest, "length": len(fake)}
		targets[product.name] = map[string]any{"custom": custom, "hashes": map[string]any{"sha256": digest}, "length": len(fake)}
	}
	root := t.TempDir()
	body, _ := json.Marshal(map[string]any{"signed": map[string]any{"targets": targets}})
	metadata := filepath.Join(root, "targets.json")
	if err := os.WriteFile(metadata, body, 0o600); err != nil {
		t.Fatal(err)
	}
	windows := filepath.Join(root, "windows")
	if out, err := exec.Command("python3", "render-installers.py", metadata, filepath.Join(root, "install"), windows).CombinedOutput(); err != nil {
		t.Fatalf("render: %v: %s", err, out)
	}
	return windows
}

// runWindowsInstaller runs the rendered installer in real PowerShell with the download replaced by a copy of fake. It returns what the
// fake pb was asked to do, one call per line, and the installer's own error.
func runWindowsInstaller(t *testing.T, installer string, fake []byte, served []byte) (calls []string, runErr error, output string) {
	t.Helper()
	shell := ""
	for _, name := range []string{"pwsh", "powershell"} {
		if path, err := exec.LookPath(name); err == nil {
			shell = path
			break
		}
	}
	if shell == "" {
		t.Skip("PowerShell not installed")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the fake pb is a shell script; the Windows path is checked on a Windows machine")
	}
	dir := t.TempDir()
	pb := filepath.Join(dir, "pb")
	log := filepath.Join(dir, "calls")
	if err := os.WriteFile(pb, fake, 0o755); err != nil {
		t.Fatal(err)
	}
	servedFile := filepath.Join(dir, "served")
	if err := os.WriteFile(servedFile, served, 0o755); err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(dir, "harness.ps1")
	script := "function Invoke-WebRequest { param([string]$Uri, [string]$OutFile, [switch]$UseBasicParsing, $TimeoutSec, $MaximumRedirection)\n" +
		"  Add-Content -LiteralPath $env:FAKE_LOG -Value ('download ' + $Uri); Copy-Item -LiteralPath $env:FAKE_SERVED -Destination $OutFile; & chmod 0755 $OutFile }\n" +
		"function Unblock-File { param($LiteralPath) }\n$ErrorActionPreference = 'Stop'\nInvoke-Expression (Get-Content -Raw -LiteralPath '" + installer + "')\n"
	if err := os.WriteFile(harness, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", harness)
	command.Env = append(os.Environ(), "FAKE_LOG="+log, "FAKE_SERVED="+servedFile, "FAKE_PB="+pb, "PAPERBOAT_ENROLLMENT_TOKEN=")
	out, err := command.CombinedOutput()
	data, _ := os.ReadFile(log)
	return strings.Fields(string(data)), err, string(out)
}

const fakePB = "#!/bin/sh\necho \"pb $*\" >> \"$FAKE_LOG\"\n" +
	"if [ \"$1\" = install ]; then printf '{\"ok\":true,\"data\":{\"executable\":\"%s\"}}' \"$FAKE_PB\"; fi\n"

func TestInstallPowerShellDownloadsVerifiesInstallsAndLogsIn(t *testing.T) {
	fake := []byte(fakePB)
	installer := renderWindowsInstaller(t, fake)
	calls, err, out := runWindowsInstaller(t, installer, fake, fake)
	joined := strings.Join(calls, " ")
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	for _, want := range []string{"download https://github.com/example/paperboat/releases/download/2026.11.09.1/pb-windows-", "pb install --json", "pb login"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls %q lack %q\n%s", joined, want, out)
		}
	}
	if strings.Index(joined, "pb install") > strings.Index(joined, "pb login") {
		t.Fatalf("login ran before install: %q", joined)
	}
}

func TestInstallPowerShellRefusesBytesThatMissTheSignedDigest(t *testing.T) {
	fake := []byte(fakePB)
	installer := renderWindowsInstaller(t, fake)
	tampered := append([]byte(nil), fake...)
	tampered[len(tampered)-2] = 'X' // same length, different digest
	calls, err, out := runWindowsInstaller(t, installer, fake, tampered)
	if err == nil || !strings.Contains(out, "digest mismatch") {
		t.Fatalf("tampered download was accepted: %v\n%s", err, out)
	}
	for _, call := range calls {
		if call == "pb" {
			t.Fatalf("tampered binary was executed: %q", calls)
		}
	}
}
