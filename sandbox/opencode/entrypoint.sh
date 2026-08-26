#!/bin/bash
# Same pattern as sandbox/claude/entrypoint.sh: run the firewall setup as
# root via the passwordless sudo rule baked into the image, then exec the
# given command (opencode's ACP mode, by default) so chorus's stdio
# piping (docker run -i) reaches it directly.
set -euo pipefail

sudo /usr/local/bin/init-firewall.sh

exec "$@"
