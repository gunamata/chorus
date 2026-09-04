# Installs chorus on Windows by downloading the matching release archive
# from GitHub Releases, verifying its checksum, and placing chorus.exe on
# PATH.
#
# Usage:
#   irm https://raw.githubusercontent.com/gunamata/chorus/main/install.ps1 | iex
#
# Also seeds ~/.chorus/agents.yaml from the release's bundled default, but
# ONLY if that file doesn't already exist -- never overwrites it on an
# upgrade, so any local edits (models, cost tiers, delegation/routing
# settings) survive across chorus versions instead of reverting to
# whatever the new binary happens to embed.
#
# Env overrides:
#   $env:CHORUS_VERSION     specific tag to install, e.g. "v0.2.0" (default: latest)
#   $env:CHORUS_INSTALL_DIR directory to install into (default: $HOME\.chorus\bin)
#   $env:CHORUS_HOME        directory the seeded agents.yaml goes into (default: $HOME\.chorus) --
#                           must match what chorus itself resolves (sessionstore.HomeDir)
#   $env:CHORUS_AGENTS      a local file path or https:// URL to seed as the central
#                           agents.yaml instead of the release's bundled default -- same
#                           local-file-or-remote-URL support as chorus's own --agents flag.
#                           Only used when no central agents.yaml exists yet (see above --
#                           never overwrites either way).

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$Repo = "gunamata/chorus"

function Write-Info($msg) { Write-Host $msg }
function Fail($msg) { Write-Error "error: $msg"; exit 1 }

# Get-VerifiedFile URL DEST NAME CHECKSUMSPATH [-Strict]
# Downloads URL to DEST and verifies it against CHECKSUMSPATH's entry for
# NAME. -Strict (the main chorus archive): any failure aborts the whole
# install via Fail. Non-strict (the optional agents.yaml seed): a
# failure -- e.g. $env:CHORUS_VERSION pinned to an older release published
# before agents.yaml existed as an asset -- just warns and returns $false,
# since seeding the central config is a convenience, never a requirement
# (chorus falls back to its embedded default with no central file present).
function Get-VerifiedFile($Url, $Dest, $Name, $ChecksumsPath, [switch]$Strict) {
    try {
        Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $Dest
    } catch {
        if ($Strict) { Fail "download failed: $Url" }
        Write-Info "warning: couldn't download $Name ($Url) -- skipping"
        return $false
    }
    $checksumLine = Select-String -Path $ChecksumsPath -Pattern ([regex]::Escape($Name)) | Select-Object -First 1
    if (-not $checksumLine) {
        if ($Strict) { Fail "no checksum entry found for $Name in checksums.txt" }
        Write-Info "warning: no checksum entry found for $Name -- skipping"
        return $false
    }
    $expected = ($checksumLine.Line -split '\s+')[0]
    $actual = (Get-FileHash -Path $Dest -Algorithm SHA256).Hash
    if ($expected.ToLower() -ne $actual.ToLower()) {
        if ($Strict) { Fail "checksum mismatch for $Name (expected $expected, got $actual)" }
        Write-Info "warning: checksum mismatch for $Name -- skipping"
        return $false
    }
    return $true
}

# Set-CustomAgents SRC DEST CHORUSHOME WORKDIR
# Populates DEST from SRC ($env:CHORUS_AGENTS) -- SRC may be a local file
# path or an https:// URL, same as chorus's own --agents flag. http:// is
# refused, not just discouraged: DEST becomes the agents.yaml chorus execs
# `spawn` commands from unconditionally, so fetching it over plaintext
# would let an on-path attacker rewrite what runs on every future launch
# (same reasoning as fetchAgentsYAML in main.go, which enforces the
# identical restriction for --agents=<url> itself). Returns $false on any
# failure (bad path, unreachable/oversized/non-https URL) so the caller
# falls back to the bundled default instead of leaving agents.yaml
# unseeded entirely -- a bad CHORUS_AGENTS value shouldn't break the rest
# of the install.
function Set-CustomAgents($Src, $Dest, $ChorusHome, $WorkDir) {
    if ($Src -match '^https://') {
        $tmp = Join-Path $WorkDir "custom-agents.yaml"
        try {
            Invoke-WebRequest -UseBasicParsing -Uri $Src -OutFile $tmp -TimeoutSec 15
        } catch {
            Write-Info "warning: couldn't download CHORUS_AGENTS ($Src) -- falling back to the bundled default"
            return $false
        }
        if ((Get-Item $tmp).Length -gt 1048576) {
            Write-Info "warning: CHORUS_AGENTS response exceeds 1 MiB -- refusing to use it, falling back to the bundled default"
            Remove-Item $tmp -ErrorAction SilentlyContinue
            return $false
        }
        New-Item -ItemType Directory -Path $ChorusHome -Force | Out-Null
        Copy-Item -Path $tmp -Destination $Dest -Force
    } elseif ($Src -match '^http://') {
        Write-Info "warning: CHORUS_AGENTS must use https:// (got http://) -- refusing to fetch over plaintext, falling back to the bundled default"
        return $false
    } else {
        if (-not (Test-Path $Src)) {
            Write-Info "warning: CHORUS_AGENTS ($Src) not found -- falling back to the bundled default"
            return $false
        }
        New-Item -ItemType Directory -Path $ChorusHome -Force | Out-Null
        Copy-Item -Path $Src -Destination $Dest -Force
    }
    Write-Info "Wrote custom config (CHORUS_AGENTS=$Src) to $Dest"
    return $true
}

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
    if (-not $version) { Fail "couldn't determine the latest release tag -- set `$env:CHORUS_VERSION` explicitly" }
}
Write-Info "Installing chorus $version (windows/$goarch)..."

$versionNum = $version -replace '^v', ''
$archive = "chorus_${versionNum}_windows_${goarch}.zip"
$baseUrl = "https://github.com/$Repo/releases/download/$version"

$workDir = Join-Path $env:TEMP "chorus-install-$([System.Guid]::NewGuid())"
New-Item -ItemType Directory -Path $workDir | Out-Null
try {
    $checksumsPath = Join-Path $workDir "checksums.txt"
    Write-Info "Verifying checksums.txt for $version..."
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/checksums.txt" -OutFile $checksumsPath
    } catch {
        Fail "couldn't download checksums.txt for $version"
    }

    $archivePath = Join-Path $workDir $archive
    Write-Info "Downloading $archive..."
    Get-VerifiedFile "$baseUrl/$archive" $archivePath $archive $checksumsPath -Strict | Out-Null

    Write-Info "Extracting..."
    Expand-Archive -Path $archivePath -DestinationPath $workDir -Force
    $exePath = Join-Path $workDir "chorus.exe"
    if (-not (Test-Path $exePath)) { Fail "archive didn't contain a chorus.exe as expected" }

    $installDir = $env:CHORUS_INSTALL_DIR
    if (-not $installDir) { $installDir = Join-Path $HOME ".chorus\bin" }
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

    $chorusHome = $env:CHORUS_HOME
    if (-not $chorusHome) { $chorusHome = Join-Path $HOME ".chorus" }
    $agentsDest = Join-Path $chorusHome "agents.yaml"
    if (Test-Path $agentsDest) {
        Write-Info "Existing $agentsDest left untouched (never overwritten on install/upgrade)."
    } else {
        $seeded = $false
        if ($env:CHORUS_AGENTS) {
            $seeded = Set-CustomAgents $env:CHORUS_AGENTS $agentsDest $chorusHome $workDir
        }
        if (-not $seeded) {
            $agentsTemp = Join-Path $workDir "agents.yaml"
            if (Get-VerifiedFile "$baseUrl/agents.yaml" $agentsTemp "agents.yaml" $checksumsPath) {
                New-Item -ItemType Directory -Path $chorusHome -Force | Out-Null
                Copy-Item -Path $agentsTemp -Destination $agentsDest -Force
                Write-Info "Wrote default config to $agentsDest"
            }
        }
    }

    Write-Info ""
    try { & (Join-Path $installDir "chorus.exe") --version } catch {}
    Write-Info "Done. Run 'chorus' from a project directory to get started (see https://github.com/$Repo#readme)."
} finally {
    Remove-Item -Recurse -Force $workDir -ErrorAction SilentlyContinue
}
