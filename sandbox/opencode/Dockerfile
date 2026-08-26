# No official Docker image exists for opencode (checked 2026-08-26 —
# only third-party/community images, none from the sst/opencode org
# itself), so this follows the same pattern as sandbox/claude/'s adapted
# official devcontainer (default-deny iptables firewall, non-root user,
# firewall script run via entrypoint.sh before the ACP process is
# exec'd) rather than inventing a different mechanism per agent.
FROM node:20

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

# /workspace is the bind-mount point chorus's {{CWD}} templating targets.
RUN mkdir -p /workspace && chown -R node:node /workspace
WORKDIR /workspace

USER node
ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global
ENV PATH=$PATH:/usr/local/share/npm-global/bin

# Install opencode at BUILD time, not via a runtime fetch — keeps the
# runtime firewall allowlist minimal.
RUN npm install -g opencode-ai

COPY init-firewall.sh /usr/local/bin/
COPY entrypoint.sh /usr/local/bin/
USER root
RUN chmod +x /usr/local/bin/init-firewall.sh /usr/local/bin/entrypoint.sh && \
  echo "node ALL=(root) NOPASSWD: /usr/local/bin/init-firewall.sh" > /etc/sudoers.d/node-firewall && \
  echo 'Defaults:node env_keep += "CHORUS_SANDBOX_ALLOW_HOSTS"' >> /etc/sudoers.d/node-firewall && \
  chmod 0440 /etc/sudoers.d/node-firewall
USER node

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["opencode", "acp"]
