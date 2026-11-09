//go:build windows

package windowsopenssh

import (
	"strings"
	"testing"
)

func TestInventoryImportsSecurityModuleBySystemPath(t *testing.T) {
	script := inventoryPowerShell(Config{InstallRoot: `C:\\Program Files\\OpenSSH`})
	for _, required := range []string{"$env:WINDIR", "Microsoft.PowerShell.Security.psd1", "Import-Module -Name $m -ErrorAction Stop", "Get-AuthenticodeSignature"} {
		if !strings.Contains(script, required) {
			t.Fatalf("inventory script missing %q: %s", required, script)
		}
	}
}

func TestParseWingetPackageListCurrentTable(t *testing.T) {
	output := "Name    Id                        Version\r\n" +
		"------------------------------------------\r\n" +
		"OpenSSH Microsoft.OpenSSH.Preview 10.0.0.0\r\n"
	registered, version := parseWingetPackageList(output)
	if !registered || version != ApprovedVersion {
		t.Fatalf("registered=%t version=%q", registered, version)
	}
}

func TestNoInstalledWingetPackageRequiresAuthoritativeMessage(t *testing.T) {
	if !noInstalledWingetPackage("No installed package found matching input criteria.") {
		t.Fatal("authoritative package-absent response was not recognized")
	}
	for _, output := range []string{"", "source unavailable", "operation timed out"} {
		if noInstalledWingetPackage(output) {
			t.Fatalf("probe failure %q was treated as package absence", output)
		}
	}
}

// A clean Windows 11 guest threw 0x80072ee6 from the capability query and pairing aborted (2026-11-10).
func TestInventoryTreatsThrowingCapabilityQueryAsAbsent(t *testing.T) {
	script := inventoryPowerShell(Config{InstallRoot: `C:\Program Files\OpenSSH`})
	if !strings.Contains(script, "$cap=$null;try{$cap=Get-WindowsCapability -Online -Name 'OpenSSH.Server*'") || !strings.Contains(script, "}catch{$cap=$null};") {
		t.Fatalf("capability query can abort the inventory: %s", script)
	}
}
