# F14-05/08/09 on an official Windows install, run elevated on the machine after the published one-liner:
# status, check, download, settings, bundle, tampered bundle refused, offline reinstall from the bundle.
# Mirrors update_checks in official_update.py (Unix). Prints one JSON line; exits 1 on FAIL.
param([Parameter(Mandatory)][string]$ExpectRelease)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$pb = (Get-ChildItem "$env:ProgramFiles\Paperboat\users\*\bin\pb.exe" | Select-Object -First 1).FullName
$dir = Join-Path $env:TEMP "pb-official-$PID"
$steps = New-Object System.Collections.Generic.List[string]

# Run pb; returns its output as one string and leaves the exit code in $script:rc. Native stderr must not throw.
function Pb {
    $ErrorActionPreference = 'Continue'
    $out = & $pb @args 2>&1 | Out-String
    $script:rc = $LASTEXITCODE
    $out
}
function PbJson {
    $out = Pb @args
    if ($script:rc -ne 0) { throw "pb $($args -join ' ') exited $($script:rc): $($out.Trim())" }
    $doc = $out.Substring($out.IndexOf('{')) | ConvertFrom-Json
    if ($doc.ok -eq $false) { throw "pb $($args -join ' ') reported a JSON failure" }
    if ($null -ne $doc.data) { $doc.data } else { $doc }
}
function Version { (Pb --version).Trim() }
function Healthy($when) {
    $overall = (PbJson doctor --json).overall
    if ($overall -ne 'healthy') { throw "doctor is $overall $when" }
}

try {
    if (-not $pb) { throw 'Paperboat is not installed' }
    New-Item -ItemType Directory $dir | Out-Null
    $official = Version
    if (-not $official.Contains($ExpectRelease)) { throw "installed version '$official' is not $ExpectRelease" }
    Healthy 'before the checks'
    $status = PbJson update status --json
    if (-not $status.cli_version -or $status.update_available -ne $false) { throw 'update status inaccurate' }
    $steps.Add('status')
    PbJson update check --json | Out-Null
    $steps.Add('check')
    $out = Pb update download --json
    if ($script:rc -ne 0) { throw "update download failed: $($out.Trim())" }
    $steps.Add('download')
    if ($null -eq (PbJson update settings --json).automatic_updates) { throw 'update settings missing' }
    $steps.Add('settings')
    $bundle = Join-Path $dir 'official.pb-update'
    $out = Pb update bundle --output $bundle
    if ($script:rc -ne 0 -or (Get-Item $bundle).Length -le 1000) { throw "update bundle failed: $($out.Trim())" }
    $out = Pb update bundle --output $bundle
    if ($script:rc -eq 0) { throw 'update bundle overwrote an existing file' }
    $steps.Add('bundle')
    $tampered = Join-Path $dir 'tampered.pb-update'
    $raw = [IO.File]::ReadAllBytes($bundle)
    $raw[[int]($raw.Length / 2)] = $raw[[int]($raw.Length / 2)] -bxor 0xFF
    [IO.File]::WriteAllBytes($tampered, $raw)
    Pb update --from $tampered --json | Out-Null
    if ($script:rc -eq 0) { throw 'tampered bundle was accepted' }
    if ((Version) -ne $official) { throw 'tampered bundle changed the version' }
    Healthy 'after the tampered bundle'
    $steps.Add('tamper-refused')
    $out = Pb update --from $bundle --json
    if ($script:rc -ne 0) { throw "offline bundle install failed rc=$($script:rc): $($out.Trim())" }
    if ((Version) -ne $official) { throw 'offline bundle install changed the version' }
    Healthy 'after the offline bundle install'
    $steps.Add('offline-install')
    @{ check = 'windows-official-update'; status = 'PASS'; version = $official; steps = $steps } | ConvertTo-Json -Compress
} catch {
    @{ check = 'windows-official-update'; status = 'FAIL'; error = "$_"; steps = $steps } | ConvertTo-Json -Compress
    exit 1
} finally {
    Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue
}
