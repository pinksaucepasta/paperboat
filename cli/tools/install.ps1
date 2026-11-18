<# The fixed Paperboat installer: one file for every release. It finds the latest GitHub release, downloads pb with the
   release's SHA256SUMS and SHA256SUMS.sig, verifies the signature with the key embedded below, and only then trusts
   the hashes. No Paperboat server is involved. Environment (all optional): PB_TOKEN (machine enrollment token),
   PB_RECIPE (pair from a recipe token), PB_NAME (machine name), PB_VERSION (a release instead of the latest),
   PB_REPO (owner/repo that publishes the releases).
   pb.exe is not Authenticode-signed: trust comes from the release signature below. Invoke-WebRequest saves no
   Mark-of-the-Web, so this one-liner never shows a SmartScreen prompt; a pb.exe downloaded by hand in a browser may. #>
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
if ($PSVersionTable.PSEdition -eq 'Core' -and -not $IsWindows) { throw 'pb installer: this command is for Windows. On macOS and Linux paste the curl command that Add machine shows for them.' }
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
$repository = if ($env:PB_REPO) { [string]$env:PB_REPO } else { 'pinksaucepasta/paperboat' }
# The release signing key (ssh-keygen -Y verify allowed_signers line); the private half is only in CI.
$allowedSigners = 'paperboat-release namespaces="paperboat-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAonuBw7PaD+214NxAVb1UwkLzJYWq0VRmpbM2EXsBhg'
$server = [string]$env:PAPERBOAT_SERVER
$requestedVersion = if ($env:PB_VERSION) { [string]$env:PB_VERSION } else { 'latest' }
$token = [string]$env:PB_TOKEN
$tokenSource = [string]$env:PAPERBOAT_ENROLLMENT_TOKEN_FILE
$name = [string]$env:PB_NAME
Remove-Item Env:PB_TOKEN -ErrorAction SilentlyContinue
if ($repository -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') { throw 'pb installer: PB_REPO must look like owner/repo' }
if ($server -and $server -notmatch '^https://') { throw 'Paperboat server URL must use HTTPS.' }
if (-not [string]::IsNullOrWhiteSpace($token) -and -not [string]::IsNullOrWhiteSpace($tokenSource)) { throw 'Use only one Paperboat enrollment token source.' }
# PB_RECIPE pairs from a recipe token and refuses at once, before anything is installed (PLAN F26).
$recipe = -not [string]::IsNullOrWhiteSpace([string]$env:PB_RECIPE)
if ($recipe) {
  $recipeToken = [string]$env:PB_RECIPE
  if (-not $recipeToken.StartsWith('pbr_') -or $recipeToken.Length -ne 67) { throw "Paperboat: PB_RECIPE is not a recipe token. A token starts with pbr_ and is 67 characters; this value is $($recipeToken.Length). Copy it again from where you saved it." }
  if (-not [string]::IsNullOrWhiteSpace($token) -or -not [string]::IsNullOrWhiteSpace($tokenSource)) { throw 'Paperboat: a recipe install pairs from PB_RECIPE; remove the enrollment token.' }
  $principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
  if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Paperboat: installing needs an administrator PowerShell. Start PowerShell with "Run as administrator" and run the command again.' }
}
$arch = if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'Arm64') { 'arm64' } elseif ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'X64') { 'amd64' } else { throw 'Paperboat supports only Windows AMD64 and ARM64.' }
$asset = "pb-windows-$arch.exe"
$tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
if (-not [IO.Path]::IsPathRooted($tempRoot)) { throw 'Paperboat temporary directory must be absolute.' }
$dir = Join-Path $tempRoot ('Paperboat\install-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $dir | Out-Null

function Download-HttpsFile([string]$Url, [string]$Output, [int]$TimeoutSeconds, [int64]$MaximumBytes) {
  if ($Url -notmatch '^https://') { throw 'Paperboat downloads require HTTPS.' }
  Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $Output -TimeoutSec $TimeoutSeconds -MaximumRedirection 5
  if ((Get-Item -LiteralPath $Output).Length -gt $MaximumBytes) { throw "Download of $Url is larger than the allowed size." }
}
# The page GitHub redirects /releases/latest to names the newest release: .../releases/tag/<version>.
function Get-LatestVersion([string]$Repository) {
  $response = Invoke-WebRequest -UseBasicParsing -Method Head -Uri "https://github.com/$Repository/releases/latest" -TimeoutSec 60 -MaximumRedirection 5
  $base = $response.BaseResponse
  $final = if ($null -ne $base.ResponseUri) { [string]$base.ResponseUri.AbsoluteUri } else { [string]$base.RequestMessage.RequestUri.AbsoluteUri }
  if ($final -notmatch '/releases/tag/([^/?#]+)$') { throw "pb installer: $Repository has no published release yet" }
  return $Matches[1]
}
# Verifying the release signature needs ssh-keygen (the Windows OpenSSH client, installed by default on Windows 10 and 11).
function Get-SshKeygen() {
  $found = Get-Command ssh-keygen -ErrorAction SilentlyContinue
  if ($null -eq $found) {
    $system = if ($env:SystemRoot) { Join-Path $env:SystemRoot 'System32\OpenSSH\ssh-keygen.exe' } else { '' }
    if ($system -and (Test-Path -LiteralPath $system -PathType Leaf)) { return $system }
    Write-Host 'pb installer: ssh-keygen is missing; adding the Windows OpenSSH Client to verify the release signature'
    try { Add-WindowsCapability -Online -Name 'OpenSSH.Client~~~~0.0.1.0' | Out-Null } catch { throw 'pb installer: ssh-keygen is required to verify the release signature and could not be installed. Start PowerShell with "Run as administrator", or add Settings > Optional features > OpenSSH Client, then run the command again.' }
    if ($system -and (Test-Path -LiteralPath $system -PathType Leaf)) { return $system }
    throw 'pb installer: ssh-keygen is still missing after adding the OpenSSH Client; add it in Settings > Optional features and run the command again.'
  }
  return $found.Source
}
function Invoke-Paperboat([string]$FilePath, [object[]]$Arguments, [string]$Operation) {
  & $FilePath @Arguments
  if ($LASTEXITCODE -ne 0) { throw "$Operation failed with exit code $LASTEXITCODE." }
}

function Invoke-PaperboatInstall([string]$FilePath) {
  $raw = & $FilePath 'install' '--json'
  if ($LASTEXITCODE -ne 0) { throw "Paperboat installation failed with exit code $LASTEXITCODE." }
  try { $result = ([string]::Join([Environment]::NewLine, [string[]]$raw)) | ConvertFrom-Json } catch { throw 'Paperboat installation returned invalid JSON.' }
  $path = [string]$result.data.executable
  if ($result.ok -ne $true -or [string]::IsNullOrWhiteSpace($path) -or -not [IO.Path]::IsPathRooted($path)) { throw 'Paperboat installation returned an invalid executable path.' }
  if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "Installed Paperboat is unavailable at $path." }
  return $path
}

try {
  $tokenFile = $null
  if (-not [string]::IsNullOrWhiteSpace($tokenSource)) {
    if (-not [IO.Path]::IsPathRooted($tokenSource)) { throw 'Paperboat enrollment token file path must be absolute.' }
    $sourceItem = Get-Item -LiteralPath $tokenSource -Force
    if ($sourceItem.PSIsContainer -or ($sourceItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Paperboat enrollment token file must be a regular non-reparse file.' }
    $owner = (Get-Acl -LiteralPath $tokenSource).Owner
    $currentOwner = [Security.Principal.WindowsIdentity]::GetCurrent().Name
    if ($owner -ne $currentOwner) { throw 'Paperboat enrollment token file must be owned by the current user.' }
    $token = [IO.File]::ReadAllText($tokenSource).Trim()
  }
  if (-not [string]::IsNullOrWhiteSpace($token)) {
    if ($token.Length -gt 512) { throw 'Paperboat enrollment token is too large.' }
    $tokenFile = Join-Path $dir 'enrollment-token'
    [IO.File]::WriteAllText($tokenFile, $token, [Text.Encoding]::ASCII)
    $userSid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $security = New-Object Security.AccessControl.FileSecurity
    $security.SetSecurityDescriptorSddlForm('O:' + $userSid + 'D:P(A;;FA;;;SY)(A;;FA;;;' + $userSid + ')')
    Set-Acl -LiteralPath $tokenFile -AclObject $security
    $token = $null
  }
  $version = if ($requestedVersion -eq 'latest') { Get-LatestVersion $repository } else { $requestedVersion }
  if ($version -notmatch '^20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$') { throw "pb installer: $version is not a release version like 2026.11.18.1" }
  $releaseUrl = "https://github.com/$repository/releases/download/$version"
  $sums = Join-Path $dir 'SHA256SUMS'
  $sumsSignature = Join-Path $dir 'SHA256SUMS.sig'
  Download-HttpsFile "$releaseUrl/SHA256SUMS" $sums 60 1048576
  Download-HttpsFile "$releaseUrl/SHA256SUMS.sig" $sumsSignature 60 65536
  $sshKeygen = Get-SshKeygen
  $allowed = Join-Path $dir 'allowed_signers'
  [IO.File]::WriteAllText($allowed, $allowedSigners + "`n", (New-Object Text.UTF8Encoding($false)))
  # ssh-keygen reads the signed message on stdin. The bytes go in raw: Start-Process -RedirectStandardInput re-encodes them.
  $startInfo = New-Object Diagnostics.ProcessStartInfo
  $startInfo.FileName = $sshKeygen
  $startInfo.Arguments = '-Y verify -f "' + $allowed + '" -I paperboat-release -n paperboat-release -s "' + $sumsSignature + '"'
  $startInfo.UseShellExecute = $false
  $startInfo.CreateNoWindow = $true
  $startInfo.RedirectStandardInput = $true
  $startInfo.RedirectStandardOutput = $true
  $startInfo.RedirectStandardError = $true
  $verify = [Diagnostics.Process]::Start($startInfo)
  $signed = [IO.File]::ReadAllBytes($sums)
  $verify.StandardInput.BaseStream.Write($signed, 0, $signed.Length)
  $verify.StandardInput.Close()
  [void]$verify.StandardOutput.ReadToEnd()
  [void]$verify.StandardError.ReadToEnd()
  $verify.WaitForExit()
  if ($verify.ExitCode -ne 0) { throw 'pb installer: the release signature on SHA256SUMS is not valid; nothing was installed.' }
  # SHA256SUMS carries the release signature only when its first line names this version, so a signed file from another release cannot be replayed here.
  $sumLines = @(Get-Content -LiteralPath $sums)
  if ($sumLines.Count -lt 2 -or $sumLines[0] -cne "# paperboat-release $version") { throw "pb installer: SHA256SUMS is signed for a different release than $version" }
  $listed = @($sumLines | Where-Object { $_ -match '^[0-9a-f]{64}  \S+$' -and $_.Substring(66) -ceq $asset })
  if ($listed.Count -ne 1) { throw "pb installer: SHA256SUMS does not list $asset exactly once" }
  $productSha = $listed[0].Substring(0, 64)
  $download = Join-Path $dir $asset
  Download-HttpsFile "$releaseUrl/$asset" $download 300 536870912
  $downloadItem = Get-Item -LiteralPath $download -Force
  if ($downloadItem.PSIsContainer -or ($downloadItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Paperboat product must be a regular non-reparse file.' }
  if ((Get-FileHash -Algorithm SHA256 -LiteralPath $download).Hash.ToLowerInvariant() -ne $productSha) { throw 'Paperboat product digest mismatch.' }
  Unblock-File -LiteralPath $download -ErrorAction SilentlyContinue

  if ($null -ne $tokenFile) {
    # pb compares with the DNS host name (real case, full length), not the NetBIOS name (upper case, 15 characters).
    $resetRaw = & $download 'reset' '--confirmation' 'RESET PAPERBOAT' '--hostname' ([System.Net.Dns]::GetHostName()) '--enrollment-token-file' $tokenFile '--json'
    if ($LASTEXITCODE -ne 0) { throw "Paperboat fresh enrollment reset failed with exit code $LASTEXITCODE." }
    try { $resetResult = ([string]::Join([Environment]::NewLine, [string[]]$resetRaw)) | ConvertFrom-Json } catch { throw 'Paperboat fresh enrollment reset returned invalid JSON.' }
    if ($resetResult.ok -ne $true -or $resetResult.data.resume -isnot [bool] -or $resetResult.data.executable -isnot [string]) { throw 'Paperboat fresh enrollment reset returned invalid JSON.' }
    $resume = [bool]$resetResult.data.resume
    $resumeExecutable = [string]$resetResult.data.executable
    if (-not [string]::IsNullOrWhiteSpace($resumeExecutable) -and (-not [IO.Path]::IsPathRooted($resumeExecutable) -or -not (Test-Path -LiteralPath $resumeExecutable -PathType Leaf))) {
      throw 'Paperboat fresh enrollment reset returned an unavailable executable.'
    }
  } else {
    $resume = $false
    $resumeExecutable = ''
  }
  # The public command owns UAC and returns its owner-scoped canonical path.
  if ($resume) {
    if ([string]::IsNullOrWhiteSpace($resumeExecutable)) { $installedPb = $download } else { $installedPb = $resumeExecutable }
  } else {
    $installedPb = Invoke-PaperboatInstall $download
    Write-Host "Installed pb to $installedPb"
  }
  if ($recipe) {
    # pb pair reads PB_RECIPE; an already paired machine is left as it is.
    $pairArgs = @('pair')
    if ($server) { $pairArgs += @('--server', $server) }
    Invoke-Paperboat $installedPb $pairArgs 'Paperboat pairing'
  } elseif ($null -ne $tokenFile) {
    if ([string]::IsNullOrWhiteSpace($name)) { $name = [string]$env:COMPUTERNAME }
    $name = $name.Trim().ToLowerInvariant()
    # Pairing uses the installed binary and never purges an existing identity.
    $pairArgs = @('pair', '--enrollment-token-file', $tokenFile, '--name', $name)
    if ($server) { $pairArgs += @('--server', $server) }
    Invoke-Paperboat $installedPb $pairArgs 'Paperboat pairing'
  } else {
    # No token: continue into the browser approval, which adds this computer to the account.
    Invoke-Paperboat $installedPb @('login') 'Paperboat login'
  }
} finally {
  $token = $null
  if (Test-Path -LiteralPath $dir) { Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue }
}
