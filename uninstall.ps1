# Uninstalls chorus on Windows -- the reverse of install.ps1.
#
# Usage:
#   irm https://raw.githubusercontent.com/gunamata/chorus/main/uninstall.ps1 | iex
#
# By default only removes the installed binary (CHORUS_INSTALL_DIR,
# default $HOME\.chorus\bin) and its entry from your user PATH -- your
# agents.yaml, session history, and any chorus-headroom Docker
# container/volume are left alone, since those are your data, not install
# artifacts. Set $env:CHORUS_UNINSTALL_PURGE = "1" first to also remove
# all of it.
#
# Env overrides (match install.ps1's):
#   $env:CHORUS_INSTALL_DIR   where the binary was installed (default: $HOME\.chorus\bin)
#   $env:CHORUS_HOME          config/state root (default: $HOME\.chorus) -- only
#                             touched under CHORUS_UNINSTALL_PURGE
#   $env:CHORUS_UNINSTALL_PURGE = "1"  also remove CHORUS_HOME and the
#                             chorus-headroom Docker container + its data volume

function Write-Info($msg) { Write-Host $msg }

$installDir = $env:CHORUS_INSTALL_DIR
if (-not $installDir) { $installDir = Join-Path $HOME ".chorus\bin" }
$exePath = Join-Path $installDir "chorus.exe"
if (Test-Path $exePath) {
    Remove-Item $exePath -Force
    Write-Info "Removed $exePath"
    if (-not (Get-ChildItem $installDir -ErrorAction SilentlyContinue)) {
        Remove-Item $installDir -Force -ErrorAction SilentlyContinue
    }
} else {
    Write-Info "No binary found at $exePath -- already removed?"
}

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -and (($userPath -split ";") -contains $installDir)) {
    $newPath = ($userPath -split ";" | Where-Object { $_ -ne $installDir }) -join ";"
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    Write-Info "Removed $installDir from your user PATH. Restart your terminal for it to take effect."
}

$dockerCmd = Get-Command docker -ErrorAction SilentlyContinue
if ($dockerCmd) {
    docker inspect chorus-headroom *> $null
    if ($LASTEXITCODE -eq 0) {
        if ($env:CHORUS_UNINSTALL_PURGE -eq "1") {
            docker rm -f chorus-headroom *> $null
            Write-Info "Removed the chorus-headroom Docker container"
            docker volume rm chorus-headroom-data *> $null
            Write-Info "Removed the chorus-headroom-data Docker volume"
        } else {
            Write-Info "Note: the chorus-headroom Docker container is still running (not touched -- it's shared/persistent by design)."
            Write-Info "      Remove it yourself with: docker rm -f chorus-headroom; docker volume rm chorus-headroom-data"
        }
    }
}

$chorusHome = $env:CHORUS_HOME
if (-not $chorusHome) { $chorusHome = Join-Path $HOME ".chorus" }
if (Test-Path $chorusHome) {
    if ($env:CHORUS_UNINSTALL_PURGE -eq "1") {
        Remove-Item -Recurse -Force $chorusHome
        Write-Info "Removed $chorusHome (agents.yaml, session history, logs)"
    } else {
        Write-Info "Note: $chorusHome (your agents.yaml, session history, logs) was left untouched."
        Write-Info "      Remove it yourself, or re-run with `$env:CHORUS_UNINSTALL_PURGE = `"1`" for a full wipe."
    }
}

Write-Info "Done."
