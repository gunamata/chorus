# Adapted from Anthropic's own official Claude Code devcontainer
# (anthropics/claude-code/.devcontainer/Dockerfile, fetched 2026-08-26) —
# same packages, non-root user, and firewall-script wiring; base image
# bumped from upstream's node:20 to node:22 (verified by a real build
# 2026-08-26: @anthropic-ai/claude-code now declares an engines.node
# >=22.0.0 requirement upstream's own pinned node:20 no longer satisfies
# — install succeeds under npm's EBADENGINE warning either way, but
# don't ship on a version the package itself says is unsupported).
# The main difference from upstream: upstream is driven by VS Code's
# devcontainer lifecycle (postStartCommand runs the firewall script);
# this image is invoked directly via `docker run -i` by chorus, so
# entrypoint.sh (not VS Code) runs the firewall then execs the ACP
# adapter. See sandbox/claude/README.md for auth options and the
# CHORUS_SANDBOX_ALLOW_HOSTS firewall-allowlist mechanism.
FROM node:22

ARG TZ
ENV TZ="$TZ"

ARG CLAUDE_CODE_VERSION=latest

RUN apt-get update && apt-get install -y --no-install-recommends \
  less \
  git \
  procps \
  sudo \
  gnupg2 \
  iptables \
  ipset \
  iproute2 \
  dnsutils \
  aggregate \
  curl \
  jq \
  ca-certificates \
  && apt-get clean && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /usr/local/share/npm-global && \
  chown -R node:node /usr/local/share

ARG USERNAME=node

ENV DEVCONTAINER=true

# /workspace is the bind-mount point chorus's {{CWD}} templating targets
# (see agents.yaml's spawn line in README.md) — the container's only view
# of the project. /home/node/.claude is left as a plain directory, not a
# mount of the host's real ~/.claude: never give a sandboxed agent the
# host's live interactive-login state (see security note in README.md).
RUN mkdir -p /workspace /home/node/.claude && \
  chown -R node:node /workspace /home/node/.claude

WORKDIR /workspace

USER node

ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global
ENV PATH=$PATH:/usr/local/share/npm-global/bin

# Install the Claude Code CLI and the ACP adapter chorus actually spawns
# at BUILD time, not via a runtime npx fetch — keeps the runtime firewall
# allowlist minimal (package registries don't need to be reachable once
# the agent subprocess is already baked into the image).
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} @agentclientprotocol/claude-agent-acp

COPY init-firewall.sh /usr/local/bin/
COPY entrypoint.sh /usr/local/bin/
USER root
RUN chmod +x /usr/local/bin/init-firewall.sh /usr/local/bin/entrypoint.sh && \
  echo "node ALL=(root) NOPASSWD: /usr/local/bin/init-firewall.sh" > /etc/sudoers.d/node-firewall && \
  echo 'Defaults:node env_keep += "CHORUS_SANDBOX_ALLOW_HOSTS"' >> /etc/sudoers.d/node-firewall && \
  chmod 0440 /etc/sudoers.d/node-firewall
USER node

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["npx", "-y", "@agentclientprotocol/claude-agent-acp"]
