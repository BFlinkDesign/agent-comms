# Tests install-fleetd.ps1 on Windows the way a person runs it: in a fresh
# Windows PowerShell, in full language mode and in the constrained language mode
# App Control applies to unsigned scripts. Run from the repository root, after
# building dist\fleetd-windows-amd64.exe stamped fleetd-v0.0.0 and its SHA256SUMS.

$ErrorActionPreference = 'Stop'
$installer = Join-Path $PSScriptRoot 'install-fleetd.ps1'
$dist = Resolve-Path 'dist'
$failures = 0

function Fail([string]$message) {
    Write-Output "FAIL: $message"
    $script:failures++
}

function Get-UserPath {
    $ErrorActionPreference = 'Continue'
    $kind = ''
    $value = ''
    foreach ($line in (reg query 'HKCU\Environment' /v Path 2>$null)) {
        if ($line -match '^\s+Path\s+(REG_\w+)\s*(.*)$') {
            $kind = $Matches[1]
            $value = $Matches[2]
        }
    }
    return @{ Kind = $kind; Value = $value }
}

# Runs the installer in a fresh Windows PowerShell and returns its exit code. What
# it printed is shown and kept in $script:lastOutput.
function Invoke-Installer([string]$from, [string]$destination, [bool]$constrained) {
    # The installer reports failure on stderr; that is output to check here, not
    # a reason for this script to stop.
    $ErrorActionPreference = 'Continue'
    if ($constrained) { $env:__PSLockdownPolicy = '4' }
    try {
        $output = powershell -NoProfile -ExecutionPolicy Bypass -File $installer -Version 'fleetd-v0.0.0' -From $from -Destination $destination 2>&1
        $code = $LASTEXITCODE
    } finally {
        Remove-Item Env:__PSLockdownPolicy -ErrorAction SilentlyContinue
    }
    $script:lastOutput = $output | Out-String
    Write-Host $script:lastOutput
    return $code
}

# The constrained run proves something only if constrained mode really engages.
$env:__PSLockdownPolicy = '4'
$mode = powershell -NoProfile -Command '$ExecutionContext.SessionState.LanguageMode'
Remove-Item Env:__PSLockdownPolicy
if ("$mode".Trim() -ne 'ConstrainedLanguage') { Fail "could not simulate constrained language mode (got '$mode')" }

$before = Get-UserPath
Write-Output "user PATH before: $($before.Kind) $($before.Value)"

foreach ($constrained in @($false, $true)) {
    $label = if ($constrained) { 'constrained' } else { 'full' }
    $destination = Join-Path $env:RUNNER_TEMP "fleetd-bin-$label"
    foreach ($run in 1..2) {
        $code = Invoke-Installer $dist $destination $constrained
        if ($code -ne 0) { Fail "$label language mode, run ${run}: installer exited $code" }
    }
    $reported = & (Join-Path $destination 'fleetd.exe') version
    if ("$reported" -notlike 'fleetd fleetd-v0.0.0 windows/*') { Fail "$label: installed fleetd reports '$reported'" }
    $after = Get-UserPath
    if ($before.Kind -and $after.Kind -ne $before.Kind) { Fail "$label: the user PATH changed type from $($before.Kind) to $($after.Kind)" }
    $count = @(($after.Value -split ';') | Where-Object { $_ -eq $destination }).Count
    if ($count -ne 1) { Fail "$label: $destination is on the user PATH $count times after two installs, want once" }
    if ($before.Value -and -not $after.Value.StartsWith($before.Value)) { Fail "$label: the existing user PATH entries were not kept as they were" }
}

# A download that does not match SHA256SUMS must install nothing.
$tampered = Join-Path $env:RUNNER_TEMP 'tampered'
Copy-Item -Recurse -Force -Path $dist -Destination $tampered
Add-Content -Path (Join-Path $tampered 'fleetd-windows-amd64.exe') -Value ([byte[]](0)) -Encoding Byte
$destination = Join-Path $env:RUNNER_TEMP 'fleetd-bin-tampered'
$code = Invoke-Installer $tampered $destination $false
if ($code -eq 0) { Fail 'a binary that does not match SHA256SUMS was accepted' }
if ($script:lastOutput -notmatch 'checksum mismatch') { Fail 'the refusal does not say the checksum did not match' }
if (Test-Path (Join-Path $destination 'fleetd.exe')) { Fail 'a binary that does not match SHA256SUMS was installed' }

if ($failures) { throw "$failures install check(s) failed" }
Write-Output 'install-fleetd.ps1: all checks passed in full and constrained language mode'
