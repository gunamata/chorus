#!/bin/bash
# Same pattern as sandbox/claude/entrypoint.sh and sandbox/opencode/
# entrypoint.sh: run the firewall setup as root via the passwordless
# sudo rule baked into the image, then exec the real gemini binary
# (the base image's own ENTRYPOINT, which we override at the Docker
# layer — see Dockerfile) with whatever args were given (--acp by
# default, via CMD).
#
# CRITICAL: the >&2 redirect below is required, not cosmetic — learned
# the hard way building sandbox/claude/ and sandbox/opencode/ (see their
# entrypoint.sh comments / chorus-spec.md §0's 2026-08-26 entry).
# `docker run -i` forwards the container's stdout byte-for-byte into
# chorus's ACP stdio channel; init-firewall.sh's diagnostic echo output
# would corrupt the JSON-RPC handshake if left on stdout.
set -euo pipefail

sudo /usr/local/bin/init-firewall.sh >&2

exec /usr/local/share/npm-global/bin/gemini "$@"
