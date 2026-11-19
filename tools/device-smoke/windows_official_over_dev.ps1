# Release gate (S3-09): the OFFICIAL pb.exe of release N, run loose from a plain folder, installs over a DEV build (N+1 candidate) on
# a paired Windows machine: `pb install` succeeds, the version becomes the release, doctor is healthy. The update target is built by the
# supplied exe, so only a release that carries the fix passes (2026-11-19: official 2026.11.19.0 over a dev build failed "pb TUF target
# descriptor is invalid"). Run elevated, after installing the dev build. Prints one JSON line; exits 1 on FAIL.
param([Parameter(Mandatory)][string]$OfficialExe, [Parameter(Mandatory)][string]$ExpectRelease)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$pb = (Get-ChildItem "$env:ProgramFiles\Paperboat\users\*\bin\pb.exe" | Select-Object -First 1).FullName

function Pb($exe) {
    $ErrorActionPreference = 'Continue'
    $out = & $exe @args 2>&1 | Out-String
    $script:rc = $LASTEXITCODE
    $out
}
function Fail($why) {
    @{ check = 'official-over-dev'; status = 'FAIL'; detail = $why } | ConvertTo-Json -Compress
    exit 1
}

$before = (Pb $pb --version).Trim()
if ($before -notmatch 'dev') { Fail "the installed build is not a dev build ($before): the gate needs official over dev" }
$out = Pb $OfficialExe install
if ($script:rc -ne 0) { Fail ("official install over $before exited $($script:rc): " + $out.Trim().Substring([Math]::Max(0, $out.Trim().Length - 300))) }
$after = (Pb $pb --version).Trim()
if ($after -notlike "*$ExpectRelease*") { Fail "version after the install is $after, expected $ExpectRelease" }
$overall = (Pb $pb doctor --json | ConvertFrom-Json).overall
if ($overall -ne 'healthy') { Fail "doctor is $overall after the install" }
@{ check = 'official-over-dev'; status = 'PASS'; before = $before; after = $after } | ConvertTo-Json -Compress
