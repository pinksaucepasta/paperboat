//go:build windows

package windowsopenssh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func collectInventory(ctx context.Context, config Config) (InventoryRecord, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	script := inventoryPowerShell(config)
	output, err := config.Runner.Run(probeCtx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cancel()
	if err != nil {
		return InventoryRecord{}, operationFailure("could not read Windows OpenSSH inventory", ErrInstallerUnavailable, err)
	}
	var record InventoryRecord
	if err := json.Unmarshal(output, &record); err != nil {
		return InventoryRecord{}, errors.Join(ErrInstallerUnavailable, fmt.Errorf("decode OpenSSH inventory: %w", err))
	}
	// Resolve only the registered Microsoft-signed App Installer executable. Do
	// not inspect package registration by running a PATH-resolved winget.exe.
	resolveCtx, resolveCancel := context.WithTimeout(ctx, 15*time.Second)
	wingetPath, resolveErr := resolveWinget(resolveCtx, config.Runner)
	resolveCancel()
	if resolveErr != nil {
		return record, nil // no package client: ClassifyInventory judges the installed files themselves
	}
	listCtx, listCancel := context.WithTimeout(ctx, 15*time.Second)
	defer listCancel()
	// Without --source, list queried msstore too and a fresh LocalSystem winget stopped at its agreement prompt
	// (winpool, 2026-11-21); a slow source refresh falls back below like any list failure.
	wingetOutput, listErr := config.Runner.Run(listCtx, wingetPath, wingetListArgs()...)
	if listErr != nil {
		return record, nil // nothing installed, or a package client LocalSystem cannot use
	}
	record.WingetRegistered, record.WingetVersion = parseWingetPackageList(string(wingetOutput))
	return record, nil
}

func inventoryPowerShell(config Config) string {
	installRoot := quotePowerShell(config.InstallRoot)
	name := config.ServiceName
	if name == "" {
		name = ServiceName
	}
	serviceName := quotePowerShell(name)
	return strings.Join([]string{
		"$ErrorActionPreference='Stop'",
		securityModuleImport,
		"function A($p){$f=[IO.File]::Open($p,'Open','Read','Read');try{$r=[IO.BinaryReader]::new($f);if($r.ReadUInt16()-ne 0x5a4d){return ''};$f.Position=0x3c;$o=$r.ReadUInt32();$f.Position=$o;if($r.ReadUInt32()-ne 0x00004550){return ''};switch($r.ReadUInt16()){0x8664{'amd64'};0xaa64{'arm64'};default{''}}}finally{$f.Dispose()}}",
		"function B($p){$i=Get-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue;if($null -eq $i){return [pscustomobject]@{path=$p;exists=$false;regular=$false;reparse_point=$false;signature_valid=$false;publisher='';version='';architecture=''}};$s=Get-AuthenticodeSignature -LiteralPath $p;$v=$i.VersionInfo.FileVersion;[pscustomobject]@{path=$p;exists=$true;regular=(-not $i.PSIsContainer);reparse_point=(($i.Attributes -band [IO.FileAttributes]::ReparsePoint)-ne 0);signature_valid=($s.Status -eq 'Valid');publisher=([string]$s.SignerCertificate.Subject);version=([string]$v);architecture=(A $p)}}",
		"function S($n){$x=Get-CimInstance Win32_Service -Filter (\"Name='\"+$n+\"'\") -ErrorAction SilentlyContinue;if($null -eq $x){return [pscustomobject]@{name=$n;exists=$false;state='';process_id=0;path_name=''}};[pscustomobject]@{name=$n;exists=$true;state=([string]$x.State);process_id=[uint32]$x.ProcessId;path_name=([string]$x.PathName)}}",
		// The capability is data only. Its query can throw despite -ErrorAction (0x80072ee6 on a clean
		// Windows 11 guest, 2026-11-10); that counts as absent.
		"$cap=$null;try{$cap=Get-WindowsCapability -Online -Name 'OpenSSH.Server*' -ErrorAction SilentlyContinue|Where-Object {$_.State -eq 'Installed'}|Select-Object -First 1}catch{$cap=$null}",
		"$root=" + installRoot,
		"[pscustomobject]@{winget_registered=$false;winget_version='';capability_present=($null -ne $cap);system_sshd=(B (Join-Path $env:WINDIR 'System32\\OpenSSH\\sshd.exe'));program_files_sshd=(B (Join-Path $root 'sshd.exe'));system_service=(S 'sshd');paperboat_service=(S " + serviceName + ")}|ConvertTo-Json -Compress -Depth 4",
	}, ";")
}

func quotePowerShell(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func parseWingetPackageList(output string) (bool, string) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for index, field := range fields {
			if !strings.EqualFold(field, PackageID) {
				continue
			}
			if index+1 < len(fields) {
				return true, fields[index+1]
			}
			return true, ""
		}
	}
	return false, ""
}
