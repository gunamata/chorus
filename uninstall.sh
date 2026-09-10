#!/bin/sh
# Uninstalls chorus on macOS or Linux — the reverse of install.sh.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/gunamata/chorus/main/uninstall.sh | sh
#
# By default only removes the installed binary (CHORUS_INSTALL_DIR,
# default ~/.chorus/bin) — your agents.yaml, session history, and any
# chorus-headroom Docker container/volume are left alone, since those are
# your data, not install artifacts. Set CHORUS_UNINSTALL_PURGE=1 to also
# remove all of it:
#
#   curl -fsSL .../uninstall.sh | CHORUS_UNINSTALL_PURGE=1 sh
#
# Env overrides (match install.sh's):
#   CHORUS_INSTALL_DIR   where the binary was installed (default: ~/.chorus/bin)
#   CHORUS_HOME          config/state root (default: ~/.chorus) — only touched
#                        under CHORUS_UNINSTALL_PURGE=1
#   CHORUS_UNINSTALL_PURGE=1  also remove CHORUS_HOME and the chorus-headroom
#                        Docker container + its data volume, if present
set -eu

log() { printf '%s\n' "$*" >&2; }

install_dir="${CHORUS_INSTALL_DIR:-$HOME/.chorus/bin}"
if [ -f "$install_dir/chorus" ]; then
    rm -f "$install_dir/chorus"
    log "Removed $install_dir/chorus"
    rmdir "$install_dir" 2>/dev/null || true
else
    log "No binary found at $install_dir/chorus — already removed?"
fi

if command -v docker >/dev/null 2>&1 && docker inspect chorus-headroom >/dev/null 2>&1; then
    if [ "${CHORUS_UNINSTALL_PURGE:-}" = "1" ]; then
        docker rm -f chorus-headroom >/dev/null 2>&1 && log "Removed the chorus-headroom Docker container"
        docker volume rm chorus-headroom-data >/dev/null 2>&1 && log "Removed the chorus-headroom-data Docker volume"
    else
        log "Note: the chorus-headroom Docker container is still running (not touched — it's shared/persistent by design)."
        log "      Remove it yourself with: docker rm -f chorus-headroom && docker volume rm chorus-headroom-data"
    fi
fi

chorus_home="${CHORUS_HOME:-$HOME/.chorus}"
if [ "${CHORUS_UNINSTALL_PURGE:-}" = "1" ]; then
    if [ -d "$chorus_home" ]; then
        rm -rf "$chorus_home"
        log "Removed $chorus_home (agents.yaml, session history, logs)"
    fi
else
    if [ -d "$chorus_home" ]; then
        log "Note: $chorus_home (your agents.yaml, session history, logs) was left untouched."
        log "      Remove it yourself, or re-run with CHORUS_UNINSTALL_PURGE=1 for a full wipe."
    fi
fi

log "Done. If you added $install_dir to your PATH manually, remove that line from your shell profile too."
