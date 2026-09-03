#!/bin/sh
# Installs chorus on macOS or Linux by downloading the matching release
# archive from GitHub Releases, verifying its checksum, and placing the
# binary on PATH. POSIX sh, not bash, so it also runs under macOS's default
# /bin/sh and dash-based Linux distros without assuming bash is present.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/gunamata/chorus/main/install.sh | sh
#
# Env overrides:
#   CHORUS_VERSION     specific tag to install, e.g. "v0.2.0" (default: latest)
#   CHORUS_INSTALL_DIR directory to install the binary into (default: see below)
set -eu

REPO="gunamata/chorus"

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || die "'$1' is required but not found on PATH"
}
need curl
need tar

# macOS ships `shasum -a 256` instead of GNU coreutils' `sha256sum` — detect
# once and use whichever is actually present rather than assuming either.
sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        die "neither sha256sum nor shasum found — can't verify the download"
    fi
}

os=$(uname -s)
case "$os" in
    Darwin) goos=darwin ;;
    Linux)  goos=linux ;;
    *) die "unsupported OS: $os (this script covers macOS and Linux only — see install.ps1 for Windows)" ;;
esac

arch=$(uname -m)
case "$arch" in
    x86_64|amd64) goarch=amd64 ;;
    arm64|aarch64) goarch=arm64 ;;
    *) die "unsupported architecture: $arch" ;;
esac

version="${CHORUS_VERSION:-}"
if [ -z "$version" ]; then
    log "Looking up the latest chorus release..."
    version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
        | grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
    [ -n "$version" ] || die "couldn't determine the latest release tag — pass CHORUS_VERSION=vX.Y.Z explicitly"
fi
log "Installing chorus $version ($goos/$goarch)..."

version_num="${version#v}"
archive="chorus_${version_num}_${goos}_${goarch}.tar.gz"
base_url="https://github.com/$REPO/releases/download/$version"

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

log "Downloading $archive..."
curl -fsSL -o "$workdir/$archive" "$base_url/$archive" \
    || die "download failed — does release $version have a $goos/$goarch asset? ($base_url/$archive)"

log "Verifying checksum..."
curl -fsSL -o "$workdir/checksums.txt" "$base_url/checksums.txt" \
    || die "couldn't download checksums.txt for $version"
# sha256sum's own output format varies (a plain space or a
# space-then-asterisk before the filename, depending on platform/mode) —
# match by filename via awk (whitespace-delimited, so both forms line up
# in $2) rather than a fragile fixed-spacing grep.
expected=$(awk -v f="$archive" '$2 == f || $2 == "*" f {print $1}' "$workdir/checksums.txt")
[ -n "$expected" ] || die "no checksum entry found for $archive in checksums.txt"
actual=$(sha256 "$workdir/$archive")
[ "$expected" = "$actual" ] || die "checksum mismatch for $archive (expected $expected, got $actual)"

log "Extracting..."
tar -xzf "$workdir/$archive" -C "$workdir"
[ -f "$workdir/chorus" ] || die "archive didn't contain a 'chorus' binary as expected"
chmod +x "$workdir/chorus"

install_dir="${CHORUS_INSTALL_DIR:-}"
if [ -z "$install_dir" ]; then
    if [ -w /usr/local/bin ]; then
        install_dir=/usr/local/bin
    else
        install_dir="$HOME/.local/bin"
    fi
fi
mkdir -p "$install_dir"
mv "$workdir/chorus" "$install_dir/chorus"
log "Installed to $install_dir/chorus"

case ":$PATH:" in
    *":$install_dir:"*) ;;
    *)
        log ""
        log "$install_dir is not on your PATH. Add it, e.g.:"
        log "  echo 'export PATH=\"$install_dir:\$PATH\"' >> ~/.profile"
        log "then restart your shell."
        ;;
esac

log ""
"$install_dir/chorus" --version 2>/dev/null || true
log "Done. Run 'chorus' from a project directory to get started (see https://github.com/$REPO#readme)."
