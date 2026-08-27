#!/bin/bash
# Same pattern as sandbox/claude/entrypoint.sh: run the firewall setup as
# root via the passwordless sudo rule baked into the image, then exec the
# given command (opencode's ACP mode, by default) so chorus's stdio
# piping (docker run -i) reaches it directly.
#
# CRITICAL: the >&2 redirect below is required, not cosmetic — see
# sandbox/claude/entrypoint.sh's comment for the full explanation.
# Confirmed live (2026-08-26) without it: init-firewall.sh's diagnostic
# echo output lands on the container's stdout, which `docker run -i`
# forwards byte-for-byte into chorus's ACP stdio channel, corrupting the
# JSON-RPC handshake before opencode's own process even starts.
set -euo pipefail

sudo /usr/local/bin/init-firewall.sh >&2

exec "$@"
