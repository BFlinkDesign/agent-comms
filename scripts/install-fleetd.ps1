<#
.SYNOPSIS
Installs one fleetd release on this PC, after checking it against the release's SHA256SUMS.

.DESCRIPTION
Downloads fleetd for this PC's architecture with gh, refuses it unless its SHA-256
matches SHA256SUMS, copies it to -Destination as fleetd.exe, checks that it reports
the requested version, and adds -Destination to the user PATH once.

The PATH is read and written with reg.exe so its registry type is kept: a PATH of
type REG_EXPAND_SZ holding %USERPROFILE% entries stays that way, where
[Environment]::SetEnvironmentVariable would silently turn it into REG_SZ and break
those entries. Only cmdlets and signed Windows tools are used, so the script also
runs in the constrained language mode App Control applies to unsigned scripts.
CI runs it in both modes on Windows.

.EXAMPLE
gh release download fleetd-v0.1.0 -R BFlinkDesign/agent-comms -p install-fleetd.ps1 -D $env:TEMP --clobber
powershell -ExecutionPolicy Bypass -File "$env:TEMP\install-fleetd.ps1" -Version fleetd-v0.1.0
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^fleetd-v\d+\.\d+\.\d+$')]
    [string]$Version,

    [string]$Repo = 'BFlinkDesign/agent-comms',

    [string]$Destination = (Join-Path $env:USERPROFILE 'bin'),

    # A directory that already holds the release files. CI uses it to test this
    # script before a release exists; a person never needs it.
    [string]$From = ''
)

$ErrorActionPreference = 'Stop'

$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$asset = "fleetd-windows-$arch.exe"

if ($From) {
    $work = $From
} else {
    $work = Join-Path $env:TEMP $Version
    New-Item -ItemType Directory -Force -Path $work | Out-Null
    gh release download $Version --repo $Repo --pattern $asset --pattern SHA256SUMS --dir $work --clobber
    if ($LASTEXITCODE -ne 0) {
        throw "gh release download $Version failed (exit $LASTEXITCODE). Is gh installed and signed in (gh auth status)?"
    }
}

$want = ''
foreach ($line in Get-Content -Path (Join-Path $work 'SHA256SUMS')) {
    $fields = -split $line
    if ($fields.Count -eq 2 -and $fields[1] -eq $asset) { $want = $fields[0] }
}
if (-not $want) { throw "SHA256SUMS in $work has no line for $asset. Nothing was installed." }
$have = (Get-FileHash -Path (Join-Path $work $asset) -Algorithm SHA256).Hash
if ($have -ne $want) {
    throw "checksum mismatch for ${asset}: SHA256SUMS says $want, the downloaded file is $have. Nothing was installed."
}

New-Item -ItemType Directory -Force -Path $Destination | Out-Null
$exe = Join-Path $Destination 'fleetd.exe'
Copy-Item -Force -Path (Join-Path $work $asset) -Destination $exe
$reported = & $exe version
if ($LASTEXITCODE -ne 0 -or "$reported" -notlike "fleetd $Version *") {
    throw "the installed fleetd reports '$reported', expected $Version"
}

# The user PATH, exactly as stored: reg.exe does not expand %VARIABLES%.
$kind = 'REG_EXPAND_SZ'
$raw = ''
# Windows PowerShell stops on a native command's redirected stderr when the error
# preference is Stop, and reg.exe writes to stderr when there is no user PATH yet.
$ErrorActionPreference = 'Continue'
$query = reg query 'HKCU\Environment' /v Path 2>$null
$found = $LASTEXITCODE -eq 0
$ErrorActionPreference = 'Stop'
if ($found) {
    foreach ($line in $query) {
        if ($line -match '^\s+Path\s+(REG_\w+)\s*(.*)$') {
            $kind = $Matches[1]
            $raw = $Matches[2]
        }
    }
}
$present = $false
foreach ($entry in $raw -split ';') {
    if ($entry.TrimEnd('\') -eq $Destination.TrimEnd('\')) { $present = $true }
}
if (-not $present) {
    $new = if (-not $raw) { $Destination } elseif ($raw.EndsWith(';')) { "$raw$Destination" } else { "$raw;$Destination" }
    reg add 'HKCU\Environment' /v Path /t $kind /d $new /f | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "could not add $Destination to the user PATH (reg add exit $LASTEXITCODE)" }
    # setx announces the change, so terminals opened from now on see the new PATH.
    setx FLEETD_HOME $Destination | Out-Null
    $env:Path = "$env:Path;$Destination"
    Write-Output "added $Destination to your user PATH; open a new terminal to use fleetd there"
}

Write-Output "installed $reported at $exe"
