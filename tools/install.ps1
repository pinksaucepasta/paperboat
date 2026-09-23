<# The release-published script pins a verifier which authenticates the product through TUF before downloading it. #>
$ErrorActionPreference = 'Stop'
$server = if ($env:PAPERBOAT_SERVER) { [string]$env:PAPERBOAT_SERVER } else { 'https://api.pprbt.dev' }
$tufUrl = if ($env:PAPERBOAT_TUF_URL) { [string]$env:PAPERBOAT_TUF_URL } else { 'https://get.pprbt.dev/tuf' }
$bootstrapVersion = '@PAPERBOAT_BOOTSTRAP_VERSION@'
$requestedVersion = if ($env:PAPERBOAT_VERSION) { [string]$env:PAPERBOAT_VERSION } else { 'latest' }
$repo = if ($env:PAPERBOAT_GITHUB_REPOSITORY) { [string]$env:PAPERBOAT_GITHUB_REPOSITORY } else { '@PAPERBOAT_BOOTSTRAP_REPOSITORY@' }
$token = [string]$env:PAPERBOAT_ENROLLMENT_TOKEN
$tokenSource = [string]$env:PAPERBOAT_ENROLLMENT_TOKEN_FILE
$name = if ($env:PAPERBOAT_MACHINE_ALIAS) { [string]$env:PAPERBOAT_MACHINE_ALIAS } else { [string]$env:PAPERBOAT_MACHINE_NAME }
if ($server -notmatch '^https://') { throw 'Paperboat server URL must use HTTPS.' }
if ($tufUrl -notmatch '^https://') { throw 'Paperboat TUF URL must use HTTPS.' }
if ($repo -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') { throw 'Paperboat release repository is invalid.' }
if (-not [string]::IsNullOrWhiteSpace($token) -and -not [string]::IsNullOrWhiteSpace($tokenSource)) { throw 'Use only one Paperboat enrollment token source.' }
$arch = if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'Arm64') { 'arm64' } elseif ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'X64') { 'amd64' } else { throw 'Paperboat supports only Windows AMD64 and ARM64.' }
$asset = "pb-windows-$arch.exe"
$bootstrapAsset = "pb-bootstrap-windows-$arch.exe"
if ($arch -eq 'amd64') {
  $bootstrapSha = '@PAPERBOAT_BOOTSTRAP_WINDOWS_AMD64_SHA256@'
  $bootstrapLength = '@PAPERBOAT_BOOTSTRAP_WINDOWS_AMD64_LENGTH@'
} else {
  $bootstrapSha = '@PAPERBOAT_BOOTSTRAP_WINDOWS_ARM64_SHA256@'
  $bootstrapLength = '@PAPERBOAT_BOOTSTRAP_WINDOWS_ARM64_LENGTH@'
}
if ($bootstrapVersion.Contains('@PAPERBOAT_') -or $bootstrapSha.Contains('@PAPERBOAT_') -or $bootstrapLength.Contains('@PAPERBOAT_')) { throw 'Use the published Paperboat installer.' }
$tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
if (-not [IO.Path]::IsPathRooted($tempRoot)) { throw 'Paperboat temporary directory must be absolute.' }
$dir = Join-Path $tempRoot ('Paperboat\bootstrap-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $dir | Out-Null

function Download-HttpsFile([string]$Url, [string]$Output, [int]$TimeoutSeconds) {
  if ($Url -notmatch '^https://') { throw 'Paperboat downloads require HTTPS.' }
  $curl = Get-Command curl.exe -CommandType Application -ErrorAction SilentlyContinue
  if ($null -eq $curl) { throw 'Paperboat installation requires curl.exe.' }
  & $curl.Source '--silent' '--show-error' '--location' '--fail' '--connect-timeout' '20' '--max-time' ([string]$TimeoutSeconds) '--proto' '=https' '--proto-redir' '=https' '--output' $Output $Url
  if ($LASTEXITCODE -ne 0) { throw "Download failed for $Url with curl exit $LASTEXITCODE." }
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
  $bootstrapUrl = "https://github.com/$repo/releases/download/$bootstrapVersion/$bootstrapAsset"
  $verifier = Join-Path $dir $bootstrapAsset
  Download-HttpsFile $bootstrapUrl $verifier 300
  if ((Get-Item -LiteralPath $verifier).Length -ne [int64]$bootstrapLength) { throw 'Paperboat bootstrap verifier length mismatch.' }
  if ((Get-FileHash -Algorithm SHA256 -LiteralPath $verifier).Hash.ToLowerInvariant() -ne $bootstrapSha) { throw 'Paperboat bootstrap verifier digest mismatch.' }
  Unblock-File -LiteralPath $verifier -ErrorAction SilentlyContinue
  $verifiedRaw = & $verifier '--tuf-url' $tufUrl '--state-dir' (Join-Path $dir 'tuf') '--github-repository' $repo '--version' $requestedVersion
  if ($LASTEXITCODE -ne 0) { throw "Paperboat signed release verification failed with exit code $LASTEXITCODE." }
  try { $verified = ([string]::Join([Environment]::NewLine, [string[]]$verifiedRaw)) | ConvertFrom-Json } catch { throw 'Paperboat verifier returned invalid JSON.' }
  $download = [string]$verified.path
  $expectedDownload = Join-Path (Join-Path $dir 'tuf') (Join-Path 'product' $asset)
  if ($download -ne $expectedDownload -or -not (Test-Path -LiteralPath $download -PathType Leaf)) { throw 'Paperboat verifier returned an unexpected artifact.' }
  if (((Get-Item -LiteralPath $download).Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Paperboat verifier returned a reparse-point artifact.' }
  Unblock-File -LiteralPath $download -ErrorAction SilentlyContinue

  if ($null -ne $tokenFile) {
    Remove-Item Env:PAPERBOAT_ENROLLMENT_TOKEN -ErrorAction SilentlyContinue
    $resetRaw = & $download 'reset' '--confirmation' 'RESET PAPERBOAT' '--hostname' ([Environment]::MachineName) '--enrollment-token-file' $tokenFile '--json'
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
  if ($null -ne $tokenFile) {
    if ([string]::IsNullOrWhiteSpace($name)) { $name = [string]$env:COMPUTERNAME }
    $name = $name.Trim().ToLowerInvariant()
    # Pairing uses the installed binary and never purges an existing identity.
    Invoke-Paperboat $installedPb @('pair', '--server', $server, '--enrollment-token-file', $tokenFile, '--name', $name) 'Paperboat pairing'
  } else {
    Invoke-Paperboat $installedPb @('--version') 'Paperboat version check'
  }
} finally {
  $token = $null
  if (Test-Path -LiteralPath $dir) { Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue }
}
