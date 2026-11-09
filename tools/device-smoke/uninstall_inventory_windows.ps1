# Read-only Windows product inventory, run elevated after `pb uninstall` exits. Prints one
# `leftover <kind>: <detail>` line per remaining product item and `inventory: clean` when none remain.
# Mirrors uninstall_inventory.py (Unix). The Inbox under ProgramData\Paperboat\users\*\Inbox is kept on purpose.
param([string]$Inbox = '')
$ErrorActionPreference = 'SilentlyContinue'
$ProgressPreference = 'SilentlyContinue'
$left = New-Object System.Collections.Generic.List[string]
function Add-Left($kind, $detail) { $left.Add("leftover ${kind}: $detail") }

Get-Service | Where-Object { $_.Name -like 'Paperboat*' } | ForEach-Object { Add-Left 'service' "$($_.Name) $($_.Status)" }
Get-Process | Where-Object { $_.Name -eq 'pb' -or $_.Name -like 'Paperboat*' } | ForEach-Object { Add-Left 'process' "$($_.Name) pid $($_.Id)" }
Get-ScheduledTask | Where-Object { $_.TaskPath -like '*Paperboat*' -or $_.TaskName -like '*Paperboat*' } | ForEach-Object { Add-Left 'scheduled-task' "$($_.TaskPath)$($_.TaskName)" }
# Windows itself creates 'pb.exe' / 'TCP Query User{guid}path' allow rules when a program listens; those are the OS's, not ours.
Get-NetFirewallRule | Where-Object { $_.DisplayName -like '*Paperboat*' -or $_.Group -like '*Paperboat*' } | ForEach-Object { Add-Left 'firewall-rule' $_.DisplayName }
Get-DnsClientNrptRule | Where-Object { $_.Comment -like '*Paperboat*' } | ForEach-Object { Add-Left 'nrpt-rule' $_.Name }
foreach ($store in 'Cert:\LocalMachine\Root', 'Cert:\CurrentUser\Root') {
    Get-ChildItem $store | Where-Object { $_.Subject -like '*Paperboat*' } | ForEach-Object { Add-Left 'certificate' "$store $($_.Subject)" }
}
# WFP: the machine guard's provider and persistent filters carry the display name "Paperboat machine guard"; filters of
# Windows Firewall (provider MPSSVC) that merely name a pb.exe path are the OS's own and are not counted.
$wfp = Join-Path $env:TEMP "pb-wfp-$PID.xml"
netsh wfp show filters file=$wfp verbose=off | Out-Null
if (Test-Path $wfp) {
    [xml]$doc = Get-Content $wfp
    $own = @($doc.wfpdiag.filters.item | Where-Object { $_.providerKey -ne 'FWPM_PROVIDER_MPSSVC_WF' -and $_.OuterXml -match 'paperboat' })
    if ($own.Count -gt 0) { Add-Left 'wfp' "$($own.Count) filter(s) naming Paperboat outside Windows Firewall" }
    Remove-Item $wfp -Force
}
netsh wfp show state file=$wfp | Out-Null
if ((Test-Path $wfp) -and (Select-String -Path $wfp -Pattern 'Paperboat machine guard' -Quiet)) { Add-Left 'wfp' 'provider Paperboat machine guard' }
Remove-Item $wfp -Force -ErrorAction SilentlyContinue
foreach ($scope in 'Machine', 'User') {
    ([Environment]::GetEnvironmentVariable('Path', $scope) -split ';') | Where-Object { $_ -like '*Paperboat*' } | ForEach-Object { Add-Left 'path' "$scope $_" }
}
$roots = @("$env:ProgramFiles\Paperboat", "$env:ProgramData\Paperboat", "$env:LOCALAPPDATA\Paperboat", "$env:APPDATA\Paperboat")
foreach ($root in $roots) {
    if (-not (Test-Path $root)) { continue }
    Get-ChildItem $root -Recurse -Force | Where-Object {
        -not ($Inbox -and ($_.FullName -eq $Inbox -or $_.FullName.StartsWith($Inbox + '\')))
    } | Where-Object { -not $_.PSIsContainer -or -not (Get-ChildItem $_.FullName -Force | Select-Object -First 1) } | Select-Object -First 5 | ForEach-Object { Add-Left 'file' $_.FullName }
}
foreach ($key in 'HKLM:\SOFTWARE\Paperboat', 'HKCU:\SOFTWARE\Paperboat') { if (Test-Path $key) { Add-Left 'registry' $key } }

if ($left.Count -eq 0) { 'inventory: clean' } else { $left; "inventory: $($left.Count) leftovers"; exit 1 }
