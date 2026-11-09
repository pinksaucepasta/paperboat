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
if (Test-Path "$env:SystemRoot\System32\Tasks\Paperboat") { Add-Left 'scheduled-task' 'System32\Tasks\Paperboat' }
Get-NetFirewallRule | Where-Object { $_.DisplayName -like '*Paperboat*' -or $_.Group -like '*Paperboat*' } | ForEach-Object { Add-Left 'firewall-rule' $_.DisplayName }
Get-DnsClientNrptRule | Where-Object { $_.Comment -like '*Paperboat*' } | ForEach-Object { Add-Left 'nrpt-rule' $_.Name }
foreach ($store in 'Cert:\LocalMachine\Root', 'Cert:\CurrentUser\Root') {
    Get-ChildItem $store | Where-Object { $_.Subject -like '*Paperboat*' } | ForEach-Object { Add-Left 'certificate' "$store $($_.Subject)" }
}
# WFP: the machine guard's provider and persistent filters carry the display name "Paperboat machine guard".
$wfp = Join-Path $env:TEMP "pb-wfp-$PID.xml"
netsh wfp show filters file=$wfp verbose=off | Out-Null
if ((Test-Path $wfp) -and (Select-String -Path $wfp -Pattern 'Paperboat' -Quiet)) { Add-Left 'wfp' 'filter or provider named Paperboat' }
Remove-Item $wfp -Force
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
