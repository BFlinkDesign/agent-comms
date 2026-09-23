# Tests install-fleetd.ps1 on Windows the way a person runs it: in a fresh
# Windows PowerShell, in full language mode and in the constrained language mode
# App Control applies to unsigned scripts. The runner has no App Control policy,
# so the constrained runs switch the session into that mode first, the way
# about_Language_Modes describes, and a probe proves the mode really restricts.
# Run from the repository root, after building dist\fleetd-windows-amd64.exe
# stamped fleetd-v0.0.0 with its SHA256SUMS, and the same in dist-wrong stamped
# with another version.

$ErrorActionPreference = 'Stop'
$installer = Join-Path $PSScriptRoot 'install-fleetd.ps1'
$dist = Resolve-Path 'dist'
$wrong = Resolve-Path 'dist-wrong'
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

$constrain = "`$ExecutionContext.SessionState.LanguageMode = 'ConstrainedLanguage'"

# Runs the installer in a fresh Windows PowerShell and returns its exit code. What
# it printed is shown and kept in $script:lastOutput.
function Invoke-Installer([string]$from, [string]$destination, [bool]$constrained) {
    # The installer reports failure on stderr; that is output to check here, not
    # a reason for this script to stop.
    $ErrorActionPreference = 'Continue'
    if ($constrained) {
        $command = "$constrain; & '$installer' -Version 'fleetd-v0.0.0' -From '$from' -Destination '$destination'"
        $output = powershell -NoProfile -ExecutionPolicy Bypass -Command $command 2>&1
    } else {
        $output = powershell -NoProfile -ExecutionPolicy Bypass -File $installer -Version 'fleetd-v0.0.0' -From $from -Destination $destination 2>&1
    }
    $code = $LASTEXITCODE
    $script:lastOutput = $output | Out-String
    Write-Host $script:lastOutput
    return $code
}

# The constrained runs prove something only if the mode engages and restricts:
# arbitrary C# through Add-Type is one of the things it forbids.
$probe = powershell -NoProfile -Command "$constrain; `$ExecutionContext.SessionState.LanguageMode; try { Add-Type -TypeDefinition 'public class FleetdProbe {}' -ErrorAction Stop; 'Add-Type allowed' } catch { 'Add-Type blocked' }"
Write-Output "constrained probe: $probe"
if ("$probe" -notmatch 'ConstrainedLanguage' -or "$probe" -notmatch 'Add-Type blocked') {
    Fail "could not put Windows PowerShell into a restricting constrained language mode (probe said '$probe')"
}

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
    if ("$reported" -notlike 'fleetd fleetd-v0.0.0 windows/*') { Fail "${label}: installed fleetd reports '$reported'" }
    $after = Get-UserPath
    if ($before.Kind -and $after.Kind -ne $before.Kind) { Fail "${label}: the user PATH changed type from $($before.Kind) to $($after.Kind)" }
    $count = @(($after.Value -split ';') | Where-Object { $_ -eq $destination }).Count
    if ($count -ne 1) { Fail "${label}: $destination is on the user PATH $count times after two installs, want once" }
    if ($before.Value -and -not $after.Value.StartsWith($before.Value)) { Fail "${label}: the existing user PATH entries were not kept as they were" }
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

# A download that reports the wrong version must leave the working fleetd.exe as
# it was, not replace it with one that failed its check.
$destination = Join-Path $env:RUNNER_TEMP 'fleetd-bin-full'
$code = Invoke-Installer $wrong $destination $false
if ($code -eq 0) { Fail 'a binary reporting the wrong version was accepted' }
$reported = & (Join-Path $destination 'fleetd.exe') version
if ("$reported" -notlike 'fleetd fleetd-v0.0.0 windows/*') { Fail "a failed install replaced the working fleetd.exe; it now reports '$reported'" }
if (Test-Path (Join-Path $destination 'fleetd.new.exe')) { Fail 'a failed install left fleetd.new.exe behind' }

# A PATH entry written with %USERPROFILE% is the same directory as its expansion,
# so installing there must not add a second, expanded copy.
$entry = @(((Get-UserPath).Value -split ';') | Where-Object { $_ -like '%USERPROFILE%\*' }) | Select-Object -First 1
if ($entry) {
    $before = (Get-UserPath).Value
    $code = Invoke-Installer $dist ($entry -replace '%USERPROFILE%', $env:USERPROFILE) $false
    if ($code -ne 0) { Fail "installing into the existing PATH entry $entry exited $code" }
    if ((Get-UserPath).Value -ne $before) { Fail "installing into the existing PATH entry $entry changed the user PATH" }
} else {
    Write-Output 'skipped the %USERPROFILE% check: the user PATH has no such entry here'
}

# The exit code is this script's verdict. GitHub's powershell shell otherwise ends
# with the exit code of the last native command, which here is the tampered
# install that is meant to fail.
if ($failures) {
    Write-Output "$failures install check(s) failed"
    exit 1
}
Write-Output 'install-fleetd.ps1: all checks passed in full and constrained language mode'
exit 0
