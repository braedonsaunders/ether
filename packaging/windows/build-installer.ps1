# Build EtherSetup-<version>.exe on a Windows host/runner.
#   pwsh packaging/windows/build-installer.ps1 -Version 1.2.0
# Requires: Go, NSIS (makensis on PATH). Signing is optional and driven by
# env vars: WINDOWS_CERT_PFX (path), WINDOWS_CERT_PASSWORD.
param(
  [Parameter(Mandatory = $true)][string]$Version,
  [string]$Arch = "amd64"
)
$ErrorActionPreference = "Stop"
$Root = Resolve-Path (Join-Path $PSScriptRoot "..\..")
$Staging = Join-Path $Root "dist\windows\staging"
New-Item -ItemType Directory -Force $Staging | Out-Null

$ldflags = "-s -w" +
  " -X github.com/braedonsaunders/ether/internal/updater.Version=$Version" +
  " -X github.com/braedonsaunders/ether/internal/updater.Commit=$env:COMMIT" +
  " -X github.com/braedonsaunders/ether/internal/updater.PublicKeyB64=$env:ETHER_UPDATE_PUBLIC_KEY" +
  " -X github.com/braedonsaunders/ether/internal/tmdb.BundledKey=$env:ETHER_TMDB_BUNDLED_KEY"

Write-Host "==> building ether.exe + ether-tray.exe (windows/$Arch, CGO_ENABLED=0)"
$env:CGO_ENABLED = "0"; $env:GOOS = "windows"; $env:GOARCH = $Arch
Push-Location $Root
go build -trimpath -ldflags $ldflags -o "$Staging\ether.exe" ./cmd/ether
if ($LASTEXITCODE -ne 0) { throw "Server build failed" }
go build -trimpath -ldflags "$ldflags -H=windowsgui" -o "$Staging\ether-tray.exe" ./cmd/ether-tray
if ($LASTEXITCODE -ne 0) { throw "Tray build failed" }
Pop-Location

Write-Host "==> staging ffmpeg"
& bash "scripts/fetch-ffmpeg.sh" "windows-$Arch"
if ($LASTEXITCODE -ne 0) { throw "FFmpeg staging failed" }
Copy-Item "dist\ffmpeg\windows-$Arch\ffmpeg.exe" $Staging
Copy-Item "dist\ffmpeg\windows-$Arch\ffprobe.exe" $Staging
Copy-Item LICENSE, THIRD_PARTY_NOTICES.md $Staging

function Sign([string]$Path) {
  if (-not $env:WINDOWS_CERT_PFX) {
    Write-Host "==> WINDOWS_CERT_PFX unset: leaving $Path unsigned (dev build)"
    return
  }
  Write-Host "==> signing $Path"
  signtool sign /f $env:WINDOWS_CERT_PFX /p $env:WINDOWS_CERT_PASSWORD `
    /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 $Path
  if ($LASTEXITCODE -ne 0) { throw "Signing failed: $Path" }
  signtool verify /pa $Path
  if ($LASTEXITCODE -ne 0) { throw "Signature verification failed: $Path" }
}

Sign "$Staging\ether.exe"
Sign "$Staging\ether-tray.exe"

Write-Host "==> makensis"
$Out = Join-Path $Root "dist\EtherSetup-$Version.exe"
makensis "/DVERSION=$Version" "/DSTAGING=$Staging" "/DOUTFILE=$Out" `
  (Join-Path $PSScriptRoot "ether.nsi")
if ($LASTEXITCODE -ne 0) { throw "Installer build failed" }
Sign $Out
Write-Host "==> done: $Out"
