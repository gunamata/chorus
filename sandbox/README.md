# Sandboxing agents in containers

Opt-in filesystem/blast-radius containment and network-exfiltration
prevention for chorus's agent subprocesses, added 2026-08-26 — see
`chorus-spec.md` §0 for the design/research trail and `CLAUDE.md`'s
architecture notes for the small Go change (`session.Spec.WorkDir`,
`EffectiveCwd`, `{{CWD}}` templating) that makes this possible without
any other chorus code change. Off by default: a normal `agents.yaml`
entry (`spawn: ["opencode", "acp"]`) runs exactly as it always has.

## Why per-agent, not one mechanism

Each agent CLI already has its own answer to "run me sandboxed," and the
best available one differs per agent — this follows whichever is most
official for each, rather than inventing one uniform wrapper:

- **`sandbox/claude/`** — adapted directly from Anthropic's own official
  devcontainer (`anthropics/claude-code/.devcontainer/`): a default-deny
  iptables firewall inside the container, `docker run`-invoked instead
  of via VS Code's devcontainer lifecycle.
- **Gemini CLI has this built into the CLI itself** — no image lives in
  this directory for it. Set `GEMINI_SANDBOX=docker` (Gemini's own env
  var, using its official `ghcr.io/google/gemini-cli:latest` image) in
  the environment chorus is launched from; `agents.yaml`'s `spawn`
  stays `["gemini", "--acp"]`, unmodified — the sandboxing and cwd
  mounting happen entirely inside Gemini's own CLI, invisible to chorus.
  **Unconfirmed by research whether `--acp`'s stdio JSON-RPC framing
  passes through this cleanly** (the mechanism is designed around normal
  interactive/scripted CLI usage) — verify live before relying on it; if
  it doesn't work, fall back to wrapping Gemini the same way as Claude/
  opencode below, still using Google's own official image directly.
- **`sandbox/opencode/`** — no official image exists for opencode, so
  this is a custom image following the same firewall pattern as
  Claude's adapted one, since there's nothing official to adopt instead.

## The one thing all three (or two, for Gemini's normal path) share

Auth and the firewall's egress allowlist are **run-time configuration,
never baked into an image or branched on in an entrypoint script** —
whoever deploys a sandboxed agent chooses direct API key, Bedrock,
Vertex AI, a corporate gateway, or (opencode's case) a private
self-hosted endpoint, via ordinary `docker run -e`/`-v` flags, exactly
as they would outside a container. The firewall's egress allowlist
extends via `CHORUS_SANDBOX_ALLOW_HOSTS` (comma-separated hostnames,
read by each image's `init-firewall.sh`) rather than requiring an image
rebuild per deployment. See each subdirectory's own README for the
specific auth options and example `agents.yaml` entries.

## Cross-platform

Rancher Desktop is the assumed container runtime on Windows (WSL2-backed),
macOS, and Linux (Lima-backed) — different VMs underneath, but the same
Docker-compatible daemon/CLI/API on all three, which is the only surface
this mechanism touches (`docker run` with capability flags, volume
mounts, env vars). No OS-specific chorus code exists or should be needed.
The one host-dependent exception is opencode's VPN-bound backend in this
deployment — see `sandbox/opencode/README.md`'s containment-verification
section for why that's a separate, harder question from the sandboxing
mechanism itself.

## Explicitly out of scope for this pass

- **Cross-agent delegation for a sandboxed agent** — the delegate-mcp
  process (spawned by the agent inside the container) can't reach
  chorus's loopback Hub on the host without extra plumbing
  (`host.docker.internal` + firewall allowlisting), and the priority
  here was containment, not delegation. A sandboxed agent's `agents.yaml`
  entry works fine standalone; just don't expect `delegate` calls
  to/from it to succeed yet.
- Windows Sandbox / hand-rolled WSL2 namespace orchestration — rejected
  in favor of just using the Docker CLI already present via Rancher
  Desktop.
