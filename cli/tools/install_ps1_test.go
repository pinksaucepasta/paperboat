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
func renderWindowsInstaller(t *testing.T, fake []byte, authenticodeMarked bool) string {
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
		if product.platform == "windows" && authenticodeMarked {
			custom["release_index"] = map[string]any{"targets": []any{map[string]any{"authenticode_signed": true}}}
		}
		targets[product.name] = map[string]any{"custom": custom, "hashes": map[string]any{"sha256": digest}, "length": len(fake)}
	}
	root := t.TempDir()
	body, _ := json.Marshal(map[string]any{"signed": map[string]any{"targets": targets}})
	metadata := filepath.Join(root, "targets.json")
	if err := os.WriteFile(metadata, body, 0o600); err != nil {
		t.Fatal(err)
	}
	windows := filepath.Join(root, "windows")
	render := exec.Command("python3", "render-installers.py", metadata, filepath.Join(root, "install"), windows)
	render.Env = append(os.Environ(), "PAPERBOAT_WINDOWS_PUBLISHER=Paperboat Test Publisher")
	if out, err := render.CombinedOutput(); err != nil {
		t.Fatalf("render: %v: %s", err, out)
	}
	return windows
}

// runWindowsInstaller runs the rendered installer in real PowerShell with the download replaced by a copy of fake. It returns what the
// fake pb was asked to do, one call per line, and the installer's own error.
func runWindowsInstaller(t *testing.T, installer string, fake []byte, served []byte, signature string) (calls []string, runErr error, output string) {
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
		"function Unblock-File { param($LiteralPath) }\n" +
		// The signature check is a Windows cmdlet: FAKE_SIG is "<status>|<signer>".
		"function Get-AuthenticodeSignature { param($LiteralPath) $p = $env:FAKE_SIG -split '[|]'; $c = New-Object psobject; " +
		"$c | Add-Member -MemberType ScriptMethod -Name GetNameInfo -Value { param($t, $f) $p[1] }.GetNewClosure(); " +
		"[pscustomobject]@{ Status = $p[0]; SignerCertificate = $c } }\n" +
		"$env:FAKE_SIGNATURE_CHECKED = ''\n$ErrorActionPreference = 'Stop'\nInvoke-Expression (Get-Content -Raw -LiteralPath '" + installer + "')\n"
	if err := os.WriteFile(harness, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", harness)
	command.Env = append(os.Environ(), "FAKE_LOG="+log, "FAKE_SERVED="+servedFile, "FAKE_PB="+pb, "PAPERBOAT_ENROLLMENT_TOKEN=", "FAKE_SIG="+signature)
	out, err := command.CombinedOutput()
	data, _ := os.ReadFile(log)
	return strings.Fields(string(data)), err, string(out)
}

const fakePB = "#!/bin/sh\necho \"pb $*\" >> \"$FAKE_LOG\"\n" +
	"if [ \"$1\" = install ]; then printf '{\"ok\":true,\"data\":{\"executable\":\"%s\"}}' \"$FAKE_PB\"; fi\n"

func TestInstallPowerShellDownloadsVerifiesInstallsAndLogsIn(t *testing.T) {
	fake := []byte(fakePB)
	installer := renderWindowsInstaller(t, fake, false)
	calls, err, out := runWindowsInstaller(t, installer, fake, fake, "NotSigned|")
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
	installer := renderWindowsInstaller(t, fake, false)
	tampered := append([]byte(nil), fake...)
	tampered[len(tampered)-2] = 'X' // same length, different digest
	calls, err, out := runWindowsInstaller(t, installer, fake, tampered, "Valid|Paperboat Test Publisher")
	if err == nil || !strings.Contains(out, "digest mismatch") {
		t.Fatalf("tampered download was accepted: %v\n%s", err, out)
	}
	for _, call := range calls {
		if call == "pb" {
			t.Fatalf("tampered binary was executed: %q", calls)
		}
	}
}

// Unmarked, the signature is never consulted (the digest is the trust root); marked, the installer needs a valid
// signature from exactly the expected publisher before it runs anything.
func TestInstallPowerShellChecksAuthenticodeOnlyWhenTheSignedMetadataMarksIt(t *testing.T) {
	fake := []byte(fakePB)
	for name, test := range map[string]struct {
		marked    bool
		signature string
		installs  bool
	}{
		"unmarked, unsigned binary installs":        {false, "NotSigned|", true},
		"unmarked, signature ignored even if bad":   {false, "HashMismatch|Someone Else", true},
		"marked, valid and expected publisher":      {true, "Valid|Paperboat Test Publisher", true},
		"marked, unsigned binary refused":           {true, "NotSigned|", false},
		"marked, tampered after signing refused":    {true, "HashMismatch|Paperboat Test Publisher", false},
		"marked, valid signature of another signer": {true, "Valid|Evil Paperboat Test Publisher", false},
	} {
		t.Run(name, func(t *testing.T) {
			installer := renderWindowsInstaller(t, fake, test.marked)
			calls, err, out := runWindowsInstaller(t, installer, fake, fake, test.signature)
			joined := strings.Join(calls, " ")
			if test.installs && (err != nil || !strings.Contains(joined, "pb install")) {
				t.Fatalf("installer failed: %v\n%s", err, out)
			}
			if !test.installs && (err == nil || !strings.Contains(out, "Authenticode") || strings.Contains(joined, "pb install") || strings.Contains(joined, "pb login")) {
				t.Fatalf("installer ran a binary it should have refused: %v %q\n%s", err, joined, out)
			}
		})
	}
}

func TestRenderingASignedWindowsTargetNeedsTheExpectedPublisher(t *testing.T) {
	renderWindowsInstaller(t, []byte(fakePB), true) // supplies a publisher
	// Without one, the renderer refuses rather than shipping an installer that cannot name its signer.
	root := t.TempDir()
	sum := sha256.Sum256([]byte("x"))
	digest := hex.EncodeToString(sum[:])
	targets := map[string]any{}
	for _, p := range []struct{ name, platform, arch, format string }{
		{"pb-linux-amd64", "linux", "amd64", "elf"}, {"pb-linux-arm64", "linux", "arm64", "elf"}, {"pb-darwin-arm64", "darwin", "arm64", "macho"},
		{"pb-windows-amd64.exe", "windows", "amd64", "pe"}, {"pb-windows-arm64.exe", "windows", "arm64", "pe"},
	} {
		custom := map[string]any{"schema": "paperboat.tuf-asset/v1", "kind": "github-release-asset", "version": "2026.11.09.1", "platform": p.platform, "architecture": p.arch, "format": p.format, "asset_name": p.name, "repository": "example/paperboat", "url": "https://github.com/example/paperboat/releases/download/2026.11.09.1/" + p.name, "sha256": digest, "length": 1}
		if p.platform == "windows" {
			custom["release_index"] = map[string]any{"targets": []any{map[string]any{"authenticode_signed": true}}}
		}
		targets[p.name] = map[string]any{"custom": custom, "hashes": map[string]any{"sha256": digest}, "length": 1}
	}
	body, _ := json.Marshal(map[string]any{"signed": map[string]any{"targets": targets}})
	metadata := filepath.Join(root, "targets.json")
	if err := os.WriteFile(metadata, body, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("python3", "render-installers.py", metadata, filepath.Join(root, "install"), filepath.Join(root, "windows"))
	command.Env = append(os.Environ(), "PAPERBOAT_WINDOWS_PUBLISHER=")
	if out, err := command.CombinedOutput(); err == nil || !strings.Contains(string(out), "PAPERBOAT_WINDOWS_PUBLISHER") {
		t.Fatalf("signed targets rendered without a publisher: %v %s", err, out)
	}
}

// Release 2026.11.09.7, clean Windows Sandbox and victus: the one-liner stopped at "fresh enrollment reset failed with
// exit code 2". The installer confirmed the reset with the NetBIOS name (VICTUS; 26D84BEC-68A9-4, cut at 15
// characters) and pb compares with os.Hostname(), the DNS host name (Victus; 26d84bec-68a9-401c-b77e-65503ff3c2c5).
func TestInstallPowerShellConfirmsTheResetWithTheHostNamePbReads(t *testing.T) {
	const expression = "[System.Net.Dns]::GetHostName()"
	script, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "'--hostname' ("+expression+")") || strings.Contains(string(script), "MachineName") {
		t.Fatal("the installer does not confirm the reset with the DNS host name")
	}
	if runtime.GOOS != "windows" {
		return // only Windows runs this installer; there the two names must be the same string
	}
	output, err := exec.Command("powershell", "-NoProfile", "-Command", expression).Output()
	want, hostErr := os.Hostname()
	if got := strings.TrimSpace(string(output)); err != nil || hostErr != nil || got != want {
		t.Fatalf("installer host name %q (%v), pb host name %q (%v)", got, err, want, hostErr)
	}
}

// The PowerShell one-liner pasted on macOS or Linux (pwsh) stops before it downloads anything and says which command is for that machine.
func TestInstallPowerShellRefusesANonWindowsHostBeforeDownloading(t *testing.T) {
	installer := renderWindowsInstaller(t, []byte("fake pb"), false)
	text, err := os.ReadFile(installer)
	if err != nil {
		t.Fatal(err)
	}
	guard := strings.Index(string(text), "-not $IsWindows")
	download := strings.Index(string(text), "Download-HttpsFile")
	if guard < 0 || download < 0 || guard > download || !strings.Contains(string(text), "macOS and Linux paste the curl command") {
		t.Fatalf("guard at %d, first download at %d", guard, download)
	}
}
