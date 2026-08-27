#!/bin/bash
# Runs as the non-root `node` user (the container's default). Sets up the
# default-deny firewall via the passwordless sudo rule baked into the
# image (see Dockerfile), then execs whatever command was given (the ACP
# adapter, by default — see Dockerfile's CMD) so it becomes PID 1's
# replacement and chorus's stdio piping (docker run -i) reaches it
# directly, same as any other agent subprocess chorus spawns.
#
# CRITICAL: init-firewall.sh's own diagnostic echo output MUST be
# redirected off stdout (>&2 below), never left on it. `docker run -i`
# (no -t) forwards the container's stdout byte-for-byte as this
# subprocess's stdout to chorus's cmd.StdoutPipe() — the exact channel
# chorus's ACP client expects to carry nothing but JSON-RPC framing from
# the very first byte. Confirmed live (2026-08-26) that without this
# redirect, dozens of firewall setup lines ("Adding GitHub range ...",
# "Firewall configuration complete", ...) land on stdout before the ACP
# adapter ever starts, which would break the initialize handshake
# immediately — this is not a theoretical concern, it reproduces every
# run. Redirecting to stderr routes it into session.Connect's existing
# per-agent .chorus/logs/<agent>.stderr.log instead (same mechanism
# already used to keep an agent subprocess's own stderr off chorus's UI).
set -euo pipefail

sudo /usr/local/bin/init-firewall.sh >&2

exec "$@"
