package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakePB answers like pb for the calls the PowerShell installer makes and logs each one.
const fakePB = "#!/bin/sh\necho \"pb $*\" >> \"$FAKE_LOG\"\n" +
	"case \"$1\" in\n" +
	"install) printf '{\"ok\":true,\"data\":{\"executable\":\"%s\"}}' \"$FAKE_PB\" ;;\n" +
	"reset) printf '{\"ok\":true,\"data\":{\"resume\":false,\"executable\":\"\"}}' ;;\n" +
	"esac\n"

func findPwsh(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"pwsh", "powershell"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Skip("PowerShell not installed")
	return ""
}

// windowsRelease is a signed fake release holding both Windows products.
func windowsRelease(t *testing.T) *fakeRelease {
	t.Helper()
	r := newFakeRelease(t, "Linux", "x86_64", "pb-windows-amd64.exe", fakePB)
	r.addAsset(t, "pb-windows-arm64.exe", fakePB)
	return r
}

// runWindowsInstaller runs install.ps1 in real PowerShell with GitHub replaced by the fake release. It returns the calls
// the fake pb and the fake downloads saw, one per line, and the installer's own error.
func runWindowsInstaller(t *testing.T, r *fakeRelease, noSshKeygen bool, env ...string) (calls []string, output string, runErr error) {
	t.Helper()
	shell := findPwsh(t)
	if runtime.GOOS == "windows" {
		t.Skip("the fake pb is a shell script; the Windows path is checked on a Windows machine")
	}
	log := filepath.Join(r.root, "calls")
	pb := filepath.Join(r.root, "pb")
	if err := os.WriteFile(pb, []byte(fakePB), 0o755); err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(r.root, "harness.ps1")
	script := "function Invoke-WebRequest { param([string]$Uri, [string]$OutFile, [switch]$UseBasicParsing, $TimeoutSec, $MaximumRedirection, $Method)\n" +
		"  Add-Content -LiteralPath $env:FAKE_LOG -Value ('download ' + $Uri)\n" +
		"  if ($Uri -eq ('https://github.com/' + $env:PB_REPO + '/releases/latest')) {\n" +
		"    $resolved = [uri]('https://github.com/' + $env:PB_REPO + '/releases/tag/' + $env:FAKE_LATEST)\n" +
		"    return [pscustomobject]@{ BaseResponse = [pscustomobject]@{ ResponseUri = $resolved } } }\n" +
		"  $parts = $Uri -split '/'\n" +
		"  $source = Join-Path (Join-Path $env:FAKE_RELEASES $parts[-2]) $parts[-1]\n" +
		"  if (-not (Test-Path -LiteralPath $source)) { throw ('404 ' + $Uri) }\n" +
		"  Copy-Item -LiteralPath $source -Destination $OutFile; if ($OutFile -like '*pb-windows-*') { & chmod 0755 $OutFile } }\n" +
		"function Unblock-File { param($LiteralPath) }\n" +
		"$ErrorActionPreference = 'Stop'\n"
	if noSshKeygen {
		script += "$env:PATH = '" + filepath.Join(r.root, "empty") + "'\n"
	}
	// pwsh on Linux is not Windows: the installer's OS guard (checked by its own test) is cut out so its download logic runs here.
	script += "$text = (Get-Content -Raw -LiteralPath '" + r.powershell + "') -replace '(?m)^if \\(\\$PSVersionTable\\.PSEdition -eq ''Core'' -and -not \\$IsWindows\\).*$', ''\nInvoke-Expression $text\n"
	if err := os.WriteFile(harness, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", harness)
	command.Env = append(os.Environ(), "FAKE_LOG="+log, "FAKE_PB="+pb, "FAKE_RELEASES="+r.releases, "FAKE_LATEST="+r.version, "PB_REPO="+fakeRepo,
		"PB_TOKEN=", "PB_RECIPE=", "PB_VERSION=", "PB_NAME=", "TMPDIR="+r.root)
	command.Env = append(command.Env, env...)
	out, err := command.CombinedOutput()
	data, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(data)), "\n"), string(out), err
}

func TestInstallPowerShellDownloadsVerifiesInstallsAndLogsIn(t *testing.T) {
	r := windowsRelease(t)
	calls, out, err := runWindowsInstaller(t, r, false)
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	want := []string{
		"download https://github.com/" + fakeRepo + "/releases/latest",
		"download https://github.com/" + fakeRepo + "/releases/download/" + fakeVersion + "/SHA256SUMS",
		"download https://github.com/" + fakeRepo + "/releases/download/" + fakeVersion + "/SHA256SUMS.sig",
	}
	for i, line := range want {
		if calls[i] != line {
			t.Fatalf("call %d = %q, want %q\nall: %q", i, calls[i], line, calls)
		}
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "/releases/download/"+fakeVersion+"/pb-windows-") || !strings.Contains(joined, "pb install --json") || !strings.Contains(joined, "pb login") {
		t.Fatalf("calls lack the product download, install or login: %q", calls)
	}
	if strings.Index(joined, "pb install") > strings.Index(joined, "pb login") {
		t.Fatalf("login ran before install: %q", joined)
	}
}

func TestInstallPowerShellPinnedVersionSkipsTheLatestLookup(t *testing.T) {
	r := windowsRelease(t)
	calls, out, err := runWindowsInstaller(t, r, false, "PB_VERSION="+fakeVersion, "FAKE_LATEST=2030.01.01.1")
	if err != nil || strings.Contains(strings.Join(calls, "\n"), "/releases/latest") {
		t.Fatalf("pinned install: %v %q\n%s", err, calls, out)
	}
}

func TestInstallPowerShellRefusesBytesThatMissTheSignedDigest(t *testing.T) {
	r := windowsRelease(t)
	write(t, r.file("pb-windows-amd64.exe"), strings.Replace(fakePB, "echo", "ech0", 1), 0o755)
	write(t, r.file("pb-windows-arm64.exe"), strings.Replace(fakePB, "echo", "ech0", 1), 0o755)
	calls, out, err := runWindowsInstaller(t, r, false)
	if err == nil || !strings.Contains(out, "digest mismatch") {
		t.Fatalf("tampered download was accepted: %v\n%s", err, out)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "pb ") {
			t.Fatalf("tampered binary was executed: %q", calls)
		}
	}
}

// Nothing is trusted before the signature verifies: no product is downloaded and pb never runs.
func TestInstallPowerShellRefusesAReleaseWhoseSignatureIsWrong(t *testing.T) {
	for name, damage := range map[string]func(*testing.T, *fakeRelease){
		"sums edited after signing": func(t *testing.T, r *fakeRelease) {
			body, _ := os.ReadFile(r.file("SHA256SUMS"))
			write(t, r.file("SHA256SUMS"), strings.Replace(string(body), "0", "1", 1), 0o644)
		},
		"signed for another release": func(t *testing.T, r *fakeRelease) {
			older := filepath.Join(r.releases, "2026.11.01.0")
			if err := os.MkdirAll(older, 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(older, "pb-windows-amd64.exe"), fakePB, 0o755)
			write(t, filepath.Join(older, "pb-windows-arm64.exe"), fakePB, 0o755)
			r.sign(t, "2026.11.01.0", "")
			for _, file := range []string{"SHA256SUMS", "SHA256SUMS.sig"} {
				body, _ := os.ReadFile(filepath.Join(older, file))
				write(t, r.file(file), string(body), 0o644)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := windowsRelease(t)
			damage(t, r)
			calls, out, err := runWindowsInstaller(t, r, false)
			if err == nil || !(strings.Contains(out, "release signature on SHA256SUMS is not valid") || strings.Contains(out, "signed for a different release")) {
				t.Fatalf("a wrong release was accepted: %v\n%s", err, out)
			}
			for _, call := range calls {
				if strings.HasPrefix(call, "pb ") || strings.Contains(call, "/pb-windows-") {
					t.Fatalf("the product was fetched or run before the signature was trusted: %q", calls)
				}
			}
		})
	}
}

func TestInstallPowerShellStopsWhenSshKeygenIsMissing(t *testing.T) {
	r := windowsRelease(t)
	calls, out, err := runWindowsInstaller(t, r, true)
	if err == nil || !strings.Contains(out, "ssh-keygen is required to verify the release signature") {
		t.Fatalf("missing ssh-keygen was not refused: %v\n%s", err, out)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "pb ") || strings.Contains(call, "/pb-windows-") {
			t.Fatalf("nothing may be downloaded or run without verification: %q", calls)
		}
	}
}

func TestInstallPowerShellRefusesAMalformedRecipeTokenAndABadRepository(t *testing.T) {
	r := windowsRelease(t)
	for env, want := range map[string]string{
		"PB_RECIPE=pbr_short":    "PB_RECIPE is not a recipe token",
		"PB_REPO=not a repo":     "PB_REPO must look like owner/repo",
		"PB_VERSION=latest-ish":  "is not a release version",
		"PB_VERSION=2026.11.18":  "is not a release version",
		"PB_VERSION=2026.11.018": "is not a release version",
	} {
		_, out, err := runWindowsInstaller(t, r, false, env)
		if err == nil || !strings.Contains(out, want) {
			t.Fatalf("%s: err=%v\n%s", env, err, out)
		}
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
	text, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	guard := strings.Index(string(text), "-not $IsWindows")
	download := strings.Index(string(text), "Download-HttpsFile ")
	if guard < 0 || download < 0 || guard > download || !strings.Contains(string(text), "macOS and Linux paste the curl command") {
		t.Fatalf("guard at %d, first download at %d", guard, download)
	}
}

// Run for real under pwsh on macOS or Linux, the one-liner stops with the message and downloads nothing.
func TestInstallPowerShellOnANonWindowsHostSaysWhichCommandToUse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the guard is for macOS and Linux hosts")
	}
	shell, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed")
	}
	out, err := exec.Command(shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", "install.ps1").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "this command is for Windows") || strings.Contains(string(out), "Download") {
		t.Fatalf("pwsh on this host: err %v, output %q", err, out)
	}
}
