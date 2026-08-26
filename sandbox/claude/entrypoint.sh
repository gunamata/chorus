#!/bin/bash
# Runs as the non-root `node` user (the container's default). Sets up the
# default-deny firewall via the passwordless sudo rule baked into the
# image (see Dockerfile), then execs whatever command was given (the ACP
# adapter, by default — see Dockerfile's CMD) so it becomes PID 1's
# replacement and chorus's stdio piping (docker run -i) reaches it
# directly, same as any other agent subprocess chorus spawns.
set -euo pipefail

sudo /usr/local/bin/init-firewall.sh

exec "$@"
