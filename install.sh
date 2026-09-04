#!/bin/sh
# Installs chorus on macOS or Linux by downloading the matching release
# archive from GitHub Releases, verifying its checksum, and placing the
# binary on PATH. POSIX sh, not bash, so it also runs under macOS's default
# /bin/sh and dash-based Linux distros without assuming bash is present.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/gunamata/chorus/main/install.sh | sh
#
# Also seeds ~/.chorus/agents.yaml from the release's bundled default, but
# ONLY if that file doesn't already exist — never overwrites it on an
# upgrade, so any local edits (models, cost tiers, delegation/routing
# settings) survive across chorus versions instead of reverting to
# whatever the new binary happens to embed.
#
# Env overrides:
#   CHORUS_VERSION     specific tag to install, e.g. "v0.2.0" (default: latest)
#   CHORUS_INSTALL_DIR directory to install the binary into (default: ~/.chorus/bin)
#   CHORUS_HOME        directory the seeded agents.yaml goes into (default: ~/.chorus) —
#                      must match what chorus itself resolves (sessionstore.HomeDir)
#   CHORUS_AGENTS      a local file path or https:// URL to seed as the central
#                      agents.yaml instead of the release's bundled default —
#                      same local-file-or-remote-URL support as chorus's own
#                      --agents flag. Only used when no central agents.yaml
#                      exists yet (see above — never overwrites either way).
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

log "Verifying checksums.txt for $version..."
curl -fsSL -o "$workdir/checksums.txt" "$base_url/checksums.txt" \
    || die "couldn't download checksums.txt for $version"

# download_verified URL DEST NAME [strict]
# Downloads URL to DEST and verifies it against checksums.txt's entry for
# NAME (sha256sum's own output format varies — a plain space or a
# space-then-asterisk before the filename, depending on platform/mode —
# so this matches by filename via awk, whitespace-delimited, rather than
# a fragile fixed-spacing grep). "strict" (the main chorus archive): any
# failure aborts the whole install. Non-strict (the optional agents.yaml
# seed below): a failure — e.g. CHORUS_VERSION pinned to an older release
# published before agents.yaml existed as an asset — just warns and
# returns non-zero, since seeding the central config is a convenience,
# never a requirement (chorus falls back to its embedded default with no
# central file present).
download_verified() {
    url="$1"; dest="$2"; name="$3"; strict="${4:-}"
    if ! curl -fsSL -o "$dest" "$url"; then
        [ "$strict" = "strict" ] && die "download failed: $url"
        log "warning: couldn't download $name ($url) — skipping"
        return 1
    fi
    expected=$(awk -v f="$name" '$2 == f || $2 == "*" f {print $1}' "$workdir/checksums.txt")
    if [ -z "$expected" ]; then
        [ "$strict" = "strict" ] && die "no checksum entry found for $name in checksums.txt"
        log "warning: no checksum entry found for $name — skipping"
        return 1
    fi
    actual=$(sha256 "$dest")
    if [ "$expected" != "$actual" ]; then
        [ "$strict" = "strict" ] && die "checksum mismatch for $name (expected $expected, got $actual)"
        log "warning: checksum mismatch for $name — skipping"
        return 1
    fi
    return 0
}

# seed_custom_agents SRC DEST
# Populates DEST from SRC (CHORUS_AGENTS) — SRC may be a local file path
# or an https:// URL, same as chorus's own --agents flag. http:// is
# refused, not just discouraged: DEST becomes the agents.yaml chorus
# execs `spawn` commands from unconditionally, so fetching it over
# plaintext would let an on-path attacker rewrite what runs on every
# future launch (same reasoning as fetchAgentsYAML in main.go, which
# enforces the identical restriction for --agents=<url> itself). Returns
# non-zero on any failure (bad path, unreachable/oversized/non-https
# URL) so the caller falls back to the bundled default instead of
# leaving agents.yaml unseeded entirely — a bad CHORUS_AGENTS value
# shouldn't break the rest of the install.
seed_custom_agents() {
    src="$1"; dest="$2"
    case "$src" in
        https://*)
            if ! curl -fsSL --max-time 15 --max-filesize 1048576 -o "$workdir/custom-agents.yaml" "$src"; then
                log "warning: couldn't download CHORUS_AGENTS ($src) — falling back to the bundled default"
                return 1
            fi
            mkdir -p "$(dirname "$dest")"
            mv "$workdir/custom-agents.yaml" "$dest"
            ;;
        http://*)
            log "warning: CHORUS_AGENTS must use https:// (got http://) — refusing to fetch over plaintext, falling back to the bundled default"
            return 1
            ;;
        *)
            if [ ! -f "$src" ]; then
                log "warning: CHORUS_AGENTS ($src) not found — falling back to the bundled default"
                return 1
            fi
            mkdir -p "$(dirname "$dest")"
            cp "$src" "$dest"
            ;;
    esac
    log "Wrote custom config (CHORUS_AGENTS=$src) to $dest"
    return 0
}

log "Downloading $archive..."
download_verified "$base_url/$archive" "$workdir/$archive" "$archive" strict

log "Extracting..."
tar -xzf "$workdir/$archive" -C "$workdir"
[ -f "$workdir/chorus" ] || die "archive didn't contain a 'chorus' binary as expected"
chmod +x "$workdir/chorus"

install_dir="${CHORUS_INSTALL_DIR:-$HOME/.chorus/bin}"
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

chorus_home="${CHORUS_HOME:-$HOME/.chorus}"
agents_dest="$chorus_home/agents.yaml"
if [ -f "$agents_dest" ]; then
    log "Existing $agents_dest left untouched (never overwritten on install/upgrade)."
elif [ -n "${CHORUS_AGENTS:-}" ] && seed_custom_agents "$CHORUS_AGENTS" "$agents_dest"; then
    : # seeded from CHORUS_AGENTS
elif download_verified "$base_url/agents.yaml" "$workdir/agents.yaml" "agents.yaml"; then
    mkdir -p "$chorus_home"
    mv "$workdir/agents.yaml" "$agents_dest"
    log "Wrote default config to $agents_dest"
fi

log ""
"$install_dir/chorus" --version 2>/dev/null || true
log "Done. Run 'chorus' from a project directory to get started (see https://github.com/$REPO#readme)."
