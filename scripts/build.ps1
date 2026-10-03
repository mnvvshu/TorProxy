<#
.SYNOPSIS
    Builds TorProxyManager.exe (GUI) and TorProxyManager-console.exe, and
    assembles a portable release folder plus an optional zip / installer.

.DESCRIPTION
    Steps:
      1. go vet + go test ./...                 (skip with -SkipTests)
      2. Generate icon PNGs + Windows resources  (skip with -NoResources)
         (icon, manifest, version info -> rsrc_windows_<arch>.syso)
      3. go build (CGO disabled, -trimpath, stripped)
      4. Assemble dist\TorProxyManager\ with configs, docs and scripts
      5. Optional: bundle Tor (-IncludeTor), zip (-Zip)

.EXAMPLE
    .\scripts\build.ps1
.EXAMPLE
    .\scripts\build.ps1 -Version 1.2.0 -Zip -IncludeTor
#>
[CmdletBinding()]
param(
    [string]$Version,
    [ValidateSet('amd64', 'arm64', '386')]
    [string]$Arch = 'amd64',
    [switch]$SkipTests,
    [switch]$NoResources,
    [switch]$IncludeTor,
    [switch]$Zip
)

$ErrorActionPreference = 'Stop'
$Root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $Root

function Step($msg) { Write-Host "`n==> $msg" -ForegroundColor Cyan }
function Invoke-Native {
    param([string]$Exe, [string[]]$Arguments)
    & $Exe @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$Exe $($Arguments -join ' ') failed with exit code $LASTEXITCODE" }
}

if (-not $Version) { $Version = (Get-Content (Join-Path $Root 'VERSION') -Raw).Trim() }
if ($Version -notmatch '^\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$') { throw "Invalid version '$Version' (expected X.Y.Z)" }
$NumericVersion = ($Version -split '[-+]')[0] + '.0'

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'Go toolchain not found. Install Go 1.22+ from https://go.dev/dl/'
}
Write-Host "TorProxyManager $Version ($Arch) - $(go version)"

$Dist = Join-Path $Root 'dist'
$Out  = Join-Path $Dist 'TorProxyManager'

# 1. Quality gates -----------------------------------------------------------
if (-not $SkipTests) {
    Step 'go vet'
    Invoke-Native go @('vet', './...')
    Step 'go test'
    Invoke-Native go @('test', '-count=1', './...')
}

# 2. Windows resources ---------------------------------------------------------
Get-ChildItem $Root -Filter 'rsrc_windows_*.syso' | Remove-Item -Force
if (-not $NoResources) {
    Step 'Generating icon and Windows resources'
    try {
        Invoke-Native go @('run', './tools/genicon', '-out', 'winres')
        Invoke-Native go @('run', 'github.com/tc-hib/go-winres@v0.3.3', 'make',
            '--in', 'winres/winres.json', '--arch', $Arch,
            '--product-version', $NumericVersion, '--file-version', $NumericVersion)
    } catch {
        Write-Warning "Resource generation failed ($_). Building without icon/version info."
        Get-ChildItem $Root -Filter 'rsrc_windows_*.syso' | Remove-Item -Force
    }
}

# 3. Compile -------------------------------------------------------------------
Step 'Compiling'
if (Test-Path $Out) { Remove-Item $Out -Recurse -Force }
New-Item -ItemType Directory -Force -Path $Out | Out-Null

$env:CGO_ENABLED = '0'
$env:GOOS = 'windows'
$env:GOARCH = $Arch
try {
    $ld = "-s -w -X torproxymanager/internal/api.AppVersion=$Version"
    Invoke-Native go @('build', '-trimpath', '-ldflags', "$ld -H windowsgui", '-o', (Join-Path $Out 'TorProxyManager.exe'), '.')
    Invoke-Native go @('build', '-trimpath', '-ldflags', $ld, '-o', (Join-Path $Out 'TorProxyManager-console.exe'), '.')
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
}

# 4. Assemble release folder ---------------------------------------------------
Step 'Assembling release folder'
New-Item -ItemType Directory -Force -Path (Join-Path $Out 'configs'), (Join-Path $Out 'scripts') | Out-Null
Copy-Item (Join-Path $Root 'configs\*') (Join-Path $Out 'configs') -Recurse
Copy-Item (Join-Path $Root 'scripts\download-tor.ps1') (Join-Path $Out 'scripts')
foreach ($f in 'README.md', 'LICENSE', 'CHANGELOG.md') {
    if (Test-Path (Join-Path $Root $f)) { Copy-Item (Join-Path $Root $f) $Out }
}
if (Test-Path (Join-Path $Root 'docs')) { Copy-Item (Join-Path $Root 'docs') $Out -Recurse }

if ($IncludeTor) {
    $torSrc = Join-Path $Root 'tor'
    if (-not (Test-Path (Join-Path $torSrc 'tor\tor.exe')) -and -not (Test-Path (Join-Path $torSrc 'tor.exe'))) {
        Step 'Downloading Tor Expert Bundle'
        & (Join-Path $Root 'scripts\download-tor.ps1') -Destination $torSrc -Arch $Arch
        if ($LASTEXITCODE -and $LASTEXITCODE -ne 0) { throw 'download-tor.ps1 failed' }
    }
    Copy-Item $torSrc (Join-Path $Out 'tor') -Recurse
}

# 5. Packaging -------------------------------------------------------------------
$artifacts = @()
if ($Zip) {
    Step 'Creating zip'
    $suffix = if ($IncludeTor) { '-with-tor' } else { '' }
    $zipPath = Join-Path $Dist "TorProxyManager-$Version-windows-$Arch$suffix.zip"
    if (Test-Path $zipPath) { Remove-Item $zipPath -Force }
    Compress-Archive -Path $Out -DestinationPath $zipPath -CompressionLevel Optimal
    $artifacts += $zipPath
}

# Checksums -------------------------------------------------------------------------
$hashTargets = @(Join-Path $Out 'TorProxyManager.exe'; Join-Path $Out 'TorProxyManager-console.exe') + $artifacts
$sums = foreach ($t in $hashTargets) {
    $h = (Get-FileHash $t -Algorithm SHA256).Hash.ToLower()
    "$h  $(Split-Path $t -Leaf)"
}
$sums | Set-Content (Join-Path $Dist 'SHA256SUMS.txt') -Encoding ASCII

Step 'Done'
Get-ChildItem $Out -File | Format-Table Name, @{n = 'Size (KB)'; e = { [math]::Round($_.Length / 1KB) } } -AutoSize
$artifacts | ForEach-Object { Write-Host "  artifact: $_" -ForegroundColor Green }
Write-Host "  release folder: $Out" -ForegroundColor Green
if (-not (Test-Path (Join-Path $Out 'tor'))) {
    Write-Host "`nNote: Tor is not bundled. Run scripts\download-tor.ps1 inside the release folder," -ForegroundColor Yellow
    Write-Host "      or rebuild with -IncludeTor, or set tor_executable_path in config.json." -ForegroundColor Yellow
}
