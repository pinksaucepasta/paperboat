<# The trusted first-party HTTPS installer pins the immutable product bytes.
   Installed updates independently verify TUF metadata and rollback policy.
   pb.exe is not Authenticode-signed: trust comes from the signed release metadata and the SHA-256 pinned
   below. Invoke-WebRequest saves no Mark-of-the-Web, so this one-liner never shows a SmartScreen prompt;
   a pb.exe downloaded by hand in a browser may. #>
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
if ($PSVersionTable.PSEdition -eq 'Core' -and -not $IsWindows) { throw 'pb installer: this command is for Windows. On macOS and Linux paste the curl command that Add machine shows for them.' }
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
$server = [string]$env:PAPERBOAT_SERVER
$requestedVersion = if ($env:PAPERBOAT_VERSION) { [string]$env:PAPERBOAT_VERSION } else { 'latest' }
$token = [string]$env:PAPERBOAT_ENROLLMENT_TOKEN
$tokenSource = [string]$env:PAPERBOAT_ENROLLMENT_TOKEN_FILE
$name = if ($env:PAPERBOAT_MACHINE_ALIAS) { [string]$env:PAPERBOAT_MACHINE_ALIAS } else { [string]$env:PAPERBOAT_MACHINE_NAME }
if ($server -and $server -notmatch '^https://') { throw 'Paperboat server URL must use HTTPS.' }
if (-not [string]::IsNullOrWhiteSpace($token) -and -not [string]::IsNullOrWhiteSpace($tokenSource)) { throw 'Use only one Paperboat enrollment token source.' }
$arch = if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'Arm64') { 'arm64' } elseif ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'X64') { 'amd64' } else { throw 'Paperboat supports only Windows AMD64 and ARM64.' }
$asset = "pb-windows-$arch.exe"
if ($arch -eq 'amd64') {
  $productVersion = '@PAPERBOAT_PRODUCT_WINDOWS_AMD64_VERSION@'
  $productUrl = '@PAPERBOAT_PRODUCT_WINDOWS_AMD64_URL@'
  $productSha = '@PAPERBOAT_PRODUCT_WINDOWS_AMD64_SHA256@'
  $productLength = '@PAPERBOAT_PRODUCT_WINDOWS_AMD64_LENGTH@'
  $productAuthenticode = '@PAPERBOAT_PRODUCT_WINDOWS_AMD64_AUTHENTICODE@'
} else {
  $productVersion = '@PAPERBOAT_PRODUCT_WINDOWS_ARM64_VERSION@'
  $productUrl = '@PAPERBOAT_PRODUCT_WINDOWS_ARM64_URL@'
  $productSha = '@PAPERBOAT_PRODUCT_WINDOWS_ARM64_SHA256@'
  $productLength = '@PAPERBOAT_PRODUCT_WINDOWS_ARM64_LENGTH@'
  $productAuthenticode = '@PAPERBOAT_PRODUCT_WINDOWS_ARM64_AUTHENTICODE@'
}
if ($productVersion -notmatch '^20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$' -or $productSha -cnotmatch '^[0-9a-f]{64}$' -or $productLength -notmatch '^[1-9][0-9]*$') { throw 'Use the published Paperboat installer.' }
if ([int64]$productLength -gt 536870912) { throw 'Paperboat product length exceeds the release bound.' }
$expectedUrl = '^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/releases/download/' + [regex]::Escape($productVersion) + '/' + [regex]::Escape($asset) + '$'
if ($productUrl -cnotmatch $expectedUrl) { throw 'Paperboat product URL does not match its immutable release identity.' }
if ($requestedVersion -ne 'latest' -and $requestedVersion -ne $productVersion) { throw "Requested Paperboat version does not match this platform's published version $productVersion." }
$tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
if (-not [IO.Path]::IsPathRooted($tempRoot)) { throw 'Paperboat temporary directory must be absolute.' }
$dir = Join-Path $tempRoot ('Paperboat\install-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $dir | Out-Null

function Download-HttpsFile([string]$Url, [string]$Output, [int]$TimeoutSeconds, [int64]$MaximumBytes) {
  if ($Url -notmatch '^https://') { throw 'Paperboat downloads require HTTPS.' }
  Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $Output -TimeoutSec $TimeoutSeconds -MaximumRedirection 5
  if ((Get-Item -LiteralPath $Output).Length -gt $MaximumBytes) { throw "Download of $Url is larger than the published length." }
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
  $download = Join-Path $dir $asset
  Download-HttpsFile $productUrl $download 300 ([int64]$productLength)
  $downloadItem = Get-Item -LiteralPath $download -Force
  if ($downloadItem.PSIsContainer -or ($downloadItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Paperboat product must be a regular non-reparse file.' }
  if ($downloadItem.Length -ne [int64]$productLength) { throw 'Paperboat product length mismatch.' }
  if ((Get-FileHash -Algorithm SHA256 -LiteralPath $download).Hash.ToLowerInvariant() -ne $productSha) { throw 'Paperboat product digest mismatch.' }
  # The signed release metadata marks a signed binary; unmarked, the digest above is the trust root.
  if ($productAuthenticode -eq 'true') {
    $signature = Get-AuthenticodeSignature -LiteralPath $download
    $signer = if ($null -ne $signature.SignerCertificate) { $signature.SignerCertificate.GetNameInfo('SimpleName', $false) } else { '' }
    if ($signature.Status -ne 'Valid' -or $signer -cne '@PAPERBOAT_WINDOWS_PUBLISHER@') { throw 'Paperboat product Authenticode signature is not valid for the expected publisher.' }
  }
  Unblock-File -LiteralPath $download -ErrorAction SilentlyContinue

  if ($null -ne $tokenFile) {
    Remove-Item Env:PAPERBOAT_ENROLLMENT_TOKEN -ErrorAction SilentlyContinue
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
  if ($null -ne $tokenFile) {
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
