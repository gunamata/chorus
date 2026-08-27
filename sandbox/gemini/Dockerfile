# Built directly on top of Google's OWN official Gemini CLI sandbox
# image (confirmed live 2026-08-26 by pulling and inspecting it —
# `us-docker.pkg.dev/gemini-code-dev/gemini-cli/sandbox:<version>`, NOT
# ghcr.io/google/gemini-cli as earlier research wrongly guessed).
#
# This exists because Gemini CLI's OWN `GEMINI_SANDBOX=docker` mechanism
# does not work with `--acp` mode: its relaunch-into-sandbox logic
# appears to do a blocking read against stdin before/instead of
# attempting the relaunch, and `--acp` mode requires a persistent,
# never-EOF stdin pipe (that's what chorus provides) — the two are
# fundamentally incompatible, confirmed live: standalone `gemini --acp`
# with a real open pipe hung indefinitely with no container ever
# started, while `gemini --acp < /dev/null` (immediate EOF) DID spawn a
# container, just one that exited instantly since there was no real
# protocol traffic. See chorus-spec.md §0's 2026-08-26 entries for the
# full diagnostic trail.
#
# The fix: bypass gemini's own relaunch logic entirely. chorus invokes
# THIS image's `docker run` directly and owns the stdio piping itself
# (the same working pattern as sandbox/claude/ and sandbox/opencode/) —
# there is no relaunch step to deadlock, since the container's gemini
# process reads its first ACP message directly from the pipe chorus
# already owns.
#
# Confirmed live via `docker inspect` (2026-08-26): the base image is
# Debian 12 (bookworm), non-root `node` user (uid 1000, matching
# Claude's sandbox pattern), ENTRYPOINT is
# /usr/local/share/npm-global/bin/gemini directly (so `docker run image
# --acp` already forwards `--acp` straight to the real gemini binary),
# apt-get is available, sudo/iptables/ipset are NOT preinstalled (added
# below, same as Claude/opencode's images).
ARG GEMINI_CLI_VERSION=0.57.0
FROM us-docker.pkg.dev/gemini-code-dev/gemini-cli/sandbox:${GEMINI_CLI_VERSION}

USER root
RUN apt-get update && apt-get install -y --no-install-recommends \
  sudo \
  iptables \
  ipset \
  iproute2 \
  dnsutils \
  aggregate \
  curl \
  jq \
  ca-certificates \
  && apt-get clean && rm -rf /var/lib/apt/lists/*

# /workspace is the bind-mount point chorus's {{CWD}} templating targets
# — the base image has no such directory by default.
RUN mkdir -p /workspace && chown node:node /workspace
WORKDIR /workspace

COPY init-firewall.sh /usr/local/bin/
COPY entrypoint.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/init-firewall.sh /usr/local/bin/entrypoint.sh && \
  echo "node ALL=(root) NOPASSWD: /usr/local/bin/init-firewall.sh" > /etc/sudoers.d/node-firewall && \
  echo 'Defaults:node env_keep += "CHORUS_SANDBOX_ALLOW_HOSTS"' >> /etc/sudoers.d/node-firewall && \
  chmod 0440 /etc/sudoers.d/node-firewall
USER node

# CMD (not the base image's own ENTRYPOINT) supplies --acp as the
# default arg to our entrypoint.sh, which forwards it to the real
# gemini binary — see entrypoint.sh.
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["--acp"]
