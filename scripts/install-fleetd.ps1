<#
.SYNOPSIS
Installs one fleetd release on this PC, after checking it against the release's SHA256SUMS.

.DESCRIPTION
Downloads fleetd for this PC's architecture with gh, stages it next to the
installed copy, and only if the staged file matches SHA256SUMS and reports the
requested version does it replace fleetd.exe in -Destination; a failed install
leaves the previous fleetd.exe as it was. It then adds -Destination to the user
PATH once, and sets FLEETD_HOME with setx, which also tells running programs that
the environment changed.

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

# SHA-256 of a file. Windows PowerShell 5.1 implements Get-FileHash in script,
# and that script cannot create its hasher in constrained language mode; certutil
# is a signed Windows tool that works in any mode.
function Get-Sha256([string]$Path) {
    try {
        return (Get-FileHash -Path $Path -Algorithm SHA256 -ErrorAction Stop).Hash
    } catch {
        $out = certutil -hashfile $Path SHA256
        if ($LASTEXITCODE -ne 0) { throw "could not hash ${Path}: $out" }
        foreach ($line in $out) {
            $hex = $line -replace '\s', ''
            if ($hex -match '^[0-9a-fA-F]{64}$') { return $hex }
        }
        throw "certutil printed no SHA-256 for ${Path}: $out"
    }
}

# PROCESSOR_ARCHITEW6432 is set when an emulated x64 PowerShell runs on ARM64.
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64' -or $env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { 'arm64' } else { 'amd64' }
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

# Each line is "<hash>  <name>", or "<hash> *<name>" when sha256sum ran in
# binary mode, as it does on Windows.
$want = ''
foreach ($line in Get-Content -Path (Join-Path $work 'SHA256SUMS')) {
    $fields = -split $line
    if ($fields.Count -eq 2 -and ($fields[1] -replace '^\*', '') -eq $asset) { $want = $fields[0] }
}
if (-not $want) { throw "SHA256SUMS in $work has no line for $asset. Nothing was installed." }

# An absolute path with no trailing backslash: it is written into PATH, and a
# trailing backslash before a closing quote would break the reg.exe command line.
New-Item -ItemType Directory -Force -Path $Destination | Out-Null
$Destination = (Convert-Path -LiteralPath $Destination).TrimEnd('\')
$exe = Join-Path $Destination 'fleetd.exe'

# Check the very file that will be installed, and replace the working copy only
# once it has passed. The .exe extension lets it run for the version check.
$staged = Join-Path $Destination 'fleetd.new.exe'
Copy-Item -Force -Path (Join-Path $work $asset) -Destination $staged
$have = Get-Sha256 $staged
if ($have -ne $want) {
    Remove-Item -Force -Path $staged
    throw "checksum mismatch for ${asset}: SHA256SUMS says $want, the downloaded file is $have. Nothing was installed."
}
$reported = & $staged version
if ($LASTEXITCODE -ne 0 -or "$reported" -notlike "fleetd $Version *") {
    Remove-Item -Force -Path $staged
    throw "the downloaded fleetd reports '$reported', expected $Version. Nothing was installed."
}
Move-Item -Force -Path $staged -Destination $exe

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
    $expanded = $entry -replace '%USERPROFILE%', $env:USERPROFILE
    if ($expanded.TrimEnd('\') -eq $Destination) { $present = $true }
}
if (-not $present) {
    # reg.exe output passes through the console code page, which shows what it
    # cannot represent as "?", and Windows PowerShell passes quotes to native
    # programs unreliably. Rather than risk writing a damaged PATH, stop.
    if ($raw -match '["?]') {
        throw "your user PATH holds a character this script cannot rewrite safely. fleetd is installed at $exe; add $Destination to your user PATH by hand."
    }
    $new = if (-not $raw) { $Destination } elseif ($raw.EndsWith(';')) { "$raw$Destination" } else { "$raw;$Destination" }
    reg add 'HKCU\Environment' /v Path /t $kind /d $new /f | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "could not add $Destination to the user PATH (reg add exit $LASTEXITCODE)" }
    # setx announces the change, so terminals opened from now on see the new PATH.
    setx FLEETD_HOME $Destination | Out-Null
    $env:Path = "$env:Path;$Destination"
    Write-Output "added $Destination to your user PATH; open a new terminal to use fleetd there"
}

Write-Output "installed $reported at $exe"
