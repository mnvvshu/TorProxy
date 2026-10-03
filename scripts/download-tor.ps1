<#
.SYNOPSIS
    Downloads and verifies the official Tor Expert Bundle for Windows.

.DESCRIPTION
    - Discovers the latest stable release on dist.torproject.org (or use -Version).
    - Downloads tor-expert-bundle-windows-<arch>-<version>.tar.gz over HTTPS.
    - Verifies its SHA-256 against sha256sums-signed-build.txt.
    - If GnuPG (gpg.exe) is installed, also verifies the OpenPGP signature of the
      checksum file against the Tor Browser Developers signing key.
    - Extracts to -Destination (default: <app dir>\tor), giving tor\tor\tor.exe,
      which TorProxyManager finds automatically.

.EXAMPLE
    .\scripts\download-tor.ps1
.EXAMPLE
    .\scripts\download-tor.ps1 -Version 15.0.24 -Destination C:\Tools\tor -Force
#>
[CmdletBinding()]
param(
    [string]$Destination,
    [string]$Version,
    [ValidateSet('amd64', '386')]
    [string]$Arch = 'amd64',
    [switch]$Force,
    [switch]$RequireSignature
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'   # Invoke-WebRequest is very slow with progress bars
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 -bor [Net.ServicePointManager]::SecurityProtocol

$Base = 'https://dist.torproject.org/torbrowser'
# Tor Browser Developers (signing key) - https://support.torproject.org/tbb/how-to-verify-signature/
$SigningKeyFpr = 'EF6E286DDA85EA2A4BA7DE684E2C6E8793298290'
$TorArch = @{ amd64 = 'x86_64'; '386' = 'i686' }[$Arch]

if (-not $Destination) {
    $appDir = Split-Path $PSScriptRoot -Parent
    $Destination = Join-Path $appDir 'tor'
}
$Destination = [IO.Path]::GetFullPath($Destination)

$existing = @((Join-Path $Destination 'tor\tor.exe'), (Join-Path $Destination 'tor.exe')) | Where-Object { Test-Path $_ } | Select-Object -First 1
if ($existing -and -not $Force) {
    Write-Host "Tor already present at $existing (use -Force to re-download)." -ForegroundColor Yellow
    exit 0
}

function Get-Text($url) { (Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 60).Content }
function Test-Url($url) {
    try { Invoke-WebRequest -Uri $url -Method Head -UseBasicParsing -TimeoutSec 30 | Out-Null; $true } catch { $false }
}

# 1. Resolve version -----------------------------------------------------------
if (-not $Version) {
    Write-Host 'Looking up the latest stable Tor Expert Bundle...'
    $index = Get-Text "$Base/"
    $versions = [regex]::Matches($index, 'href="(\d+(?:\.\d+)+)/"') |
        ForEach-Object { $_.Groups[1].Value } |
        Sort-Object { [version]($_ + ('.0' * (3 - ($_.Split('.').Count - 1)))) } -Descending
    foreach ($v in $versions) {
        if (Test-Url "$Base/$v/tor-expert-bundle-windows-$TorArch-$v.tar.gz") { $Version = $v; break }
    }
    if (-not $Version) { throw "Could not find a Windows $TorArch expert bundle on $Base" }
}
$file = "tor-expert-bundle-windows-$TorArch-$Version.tar.gz"
$url = "$Base/$Version/$file"
Write-Host "Tor Expert Bundle $Version ($TorArch)" -ForegroundColor Cyan

$work = Join-Path ([IO.Path]::GetTempPath()) ("tpm-tor-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null
try {
    # 2. Download ----------------------------------------------------------------
    $archive = Join-Path $work $file
    $sums = Join-Path $work 'sha256sums-signed-build.txt'
    Write-Host "Downloading $url"
    Invoke-WebRequest -Uri $url -OutFile $archive -UseBasicParsing -TimeoutSec 600
    Invoke-WebRequest -Uri "$Base/$Version/sha256sums-signed-build.txt" -OutFile $sums -UseBasicParsing -TimeoutSec 60

    # 3. Signature (optional, requires gpg) ---------------------------------------
    $gpg = Get-Command gpg -ErrorAction SilentlyContinue
    if ($gpg) {
        Write-Host 'Verifying OpenPGP signature of the checksum file...'
        Invoke-WebRequest -Uri "$Base/$Version/sha256sums-signed-build.txt.asc" -OutFile "$sums.asc" -UseBasicParsing -TimeoutSec 60
        $gnupgHome = Join-Path $work 'gnupg'
        New-Item -ItemType Directory -Path $gnupgHome | Out-Null
        & $gpg.Source --homedir $gnupgHome --batch --quiet --auto-key-locate nodefault,wkd --locate-keys torbrowser@torproject.org 2>$null | Out-Null
        if ($LASTEXITCODE -ne 0) {
            & $gpg.Source --homedir $gnupgHome --batch --quiet --keyserver hkps://keys.openpgp.org --recv-keys $SigningKeyFpr 2>$null | Out-Null
        }
        $status = & $gpg.Source --homedir $gnupgHome --batch --status-fd 1 --verify "$sums.asc" $sums 2>$null
        # VALIDSIG <signing-(sub)key fpr> ... <primary key fpr>
        $valid = $status | Where-Object { $_ -match '^\[GNUPG:\] VALIDSIG ' -and $_ -match $SigningKeyFpr }
        if (-not $valid) { throw 'OpenPGP signature verification FAILED - do not use this download.' }
        Write-Host '  signature OK (Tor Browser Developers signing key)' -ForegroundColor Green
    } elseif ($RequireSignature) {
        throw 'gpg.exe not found but -RequireSignature was given. Install Gpg4win: https://gpg4win.org'
    } else {
        Write-Warning 'gpg.exe not found - skipping signature check (SHA-256 over HTTPS only). Install Gpg4win for full verification.'
    }

    # 4. Checksum ---------------------------------------------------------------------
    $expected = (Get-Content $sums | Where-Object { $_ -match "^([0-9a-f]{64})\s+\*?$([regex]::Escape($file))$" } | ForEach-Object { $Matches[1] }) | Select-Object -First 1
    if (-not $expected) { throw "No checksum for $file in sha256sums-signed-build.txt" }
    $actual = (Get-FileHash $archive -Algorithm SHA256).Hash.ToLower()
    if ($actual -ne $expected) { throw "SHA-256 mismatch for $file`n  expected $expected`n  actual   $actual" }
    Write-Host "  sha256 OK ($actual)" -ForegroundColor Green

    # 5. Extract ----------------------------------------------------------------------
    $tar = Get-Command tar.exe -ErrorAction SilentlyContinue
    if (-not $tar) { throw 'tar.exe not found (included with Windows 10 1803+).' }
    $extract = Join-Path $work 'x'
    New-Item -ItemType Directory -Path $extract | Out-Null
    & $tar.Source -xzf $archive -C $extract
    if ($LASTEXITCODE -ne 0) { throw 'Extraction failed' }

    if (Test-Path $Destination) { Remove-Item $Destination -Recurse -Force }
    New-Item -ItemType Directory -Force -Path $Destination | Out-Null
    Copy-Item (Join-Path $extract '*') $Destination -Recurse -Force
    Set-Content -Path (Join-Path $Destination 'VERSION.txt') -Value "Tor Expert Bundle $Version ($TorArch)`r`nsha256 $actual`r`nsource $url" -Encoding ASCII

    $torExe = Get-ChildItem $Destination -Recurse -Filter tor.exe | Select-Object -First 1
    if (-not $torExe) { throw 'tor.exe not found in the extracted bundle' }
    $ver = & $torExe.FullName --version 2>$null | Select-Object -First 1
    Write-Host "`nInstalled: $($torExe.FullName)" -ForegroundColor Green
    if ($ver) { Write-Host "           $ver" }
} finally {
    Remove-Item $work -Recurse -Force -ErrorAction SilentlyContinue
}
