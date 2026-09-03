# Installs chorus on Windows by downloading the matching release archive
# from GitHub Releases, verifying its checksum, and placing chorus.exe on
# PATH.
#
# Usage:
#   irm https://raw.githubusercontent.com/gunamata/chorus/main/install.ps1 | iex
#
# Env overrides:
#   $env:CHORUS_VERSION     specific tag to install, e.g. "v0.2.0" (default: latest)
#   $env:CHORUS_INSTALL_DIR directory to install into (default: $env:LOCALAPPDATA\chorus\bin)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$Repo = "gunamata/chorus"

function Write-Info($msg) { Write-Host $msg }
function Fail($msg) { Write-Error "error: $msg"; exit 1 }

switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { $goarch = "amd64" }
    "ARM64" { $goarch = "arm64" }
    default { Fail "unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

$version = $env:CHORUS_VERSION
if (-not $version) {
    Write-Info "Looking up the latest chorus release..."
    try {
        $release = Invoke-RestMethod -UseBasicParsing "https://api.github.com/repos/$Repo/releases/latest"
    } catch {
        Fail "couldn't reach the GitHub releases API: $_"
    }
    $version = $release.tag_name
    if (-not $version) { Fail "couldn't determine the latest release tag — set `$env:CHORUS_VERSION` explicitly" }
}
Write-Info "Installing chorus $version (windows/$goarch)..."

$versionNum = $version -replace '^v', ''
$archive = "chorus_${versionNum}_windows_${goarch}.zip"
$baseUrl = "https://github.com/$Repo/releases/download/$version"

$workDir = Join-Path $env:TEMP "chorus-install-$([System.Guid]::NewGuid())"
New-Item -ItemType Directory -Path $workDir | Out-Null
try {
    $archivePath = Join-Path $workDir $archive
    Write-Info "Downloading $archive..."
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/$archive" -OutFile $archivePath
    } catch {
        Fail "download failed — does release $version have a windows/$goarch asset? ($baseUrl/$archive)"
    }

    Write-Info "Verifying checksum..."
    $checksumsPath = Join-Path $workDir "checksums.txt"
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/checksums.txt" -OutFile $checksumsPath
    } catch {
        Fail "couldn't download checksums.txt for $version"
    }
    # sha256sum's output format varies (a plain space or a space-then-
    # asterisk before the filename) — match on the filename showing up
    # anywhere on the line rather than assuming exact spacing.
    $checksumLine = Select-String -Path $checksumsPath -Pattern ([regex]::Escape($archive)) | Select-Object -First 1
    if (-not $checksumLine) { Fail "no checksum entry found for $archive in checksums.txt" }
    $expected = ($checksumLine.Line -split '\s+')[0]
    $actual = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash
    if ($expected.ToLower() -ne $actual.ToLower()) {
        Fail "checksum mismatch for $archive (expected $expected, got $actual)"
    }

    Write-Info "Extracting..."
    Expand-Archive -Path $archivePath -DestinationPath $workDir -Force
    $exePath = Join-Path $workDir "chorus.exe"
    if (-not (Test-Path $exePath)) { Fail "archive didn't contain a chorus.exe as expected" }

    $installDir = $env:CHORUS_INSTALL_DIR
    if (-not $installDir) { $installDir = Join-Path $env:LOCALAPPDATA "chorus\bin" }
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    Copy-Item -Path $exePath -Destination (Join-Path $installDir "chorus.exe") -Force
    Write-Info "Installed to $installDir\chorus.exe"

    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (-not $userPath) { $userPath = "" }
    if (($userPath -split ";") -notcontains $installDir) {
        $newPath = if ($userPath) { "$userPath;$installDir" } else { $installDir }
        [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
        Write-Info ""
        Write-Info "Added $installDir to your user PATH. Restart your terminal for it to take effect."
    }

    Write-Info ""
    try { & (Join-Path $installDir "chorus.exe") --version } catch {}
    Write-Info "Done. Run 'chorus' from a project directory to get started (see https://github.com/$Repo#readme)."
} finally {
    Remove-Item -Recurse -Force $workDir -ErrorAction SilentlyContinue
}
