# Claude Code sandbox image

Adapted directly from Anthropic's own official devcontainer
(`anthropics/claude-code/.devcontainer/`, fetched 2026-08-26) — same
base image, packages, non-root `node` user, and default-deny iptables
firewall. The only structural change: upstream's firewall init runs via
VS Code's `postStartCommand` lifecycle hook; here `entrypoint.sh` runs it
directly since chorus invokes this image with a plain `docker run -i`,
not the VS Code devcontainer CLI.

## Build

```sh
docker build -t chorus-claude-sandbox sandbox/claude/
```

Works identically on Windows (Rancher Desktop + WSL2), macOS, and Linux
(Rancher Desktop + Lima) — this only touches the Docker CLI/daemon API,
which Rancher Desktop exposes the same way on all three.

## Auth — pick one, none are hardcoded into the image

The entrypoint never branches on a specific credential env var; it just
execs the Claude ACP adapter after the firewall is up. Whatever auth
option you use is supplied via ordinary `docker run` flags, same as
running Claude Code outside a container:

- **Direct API** (simplest, matches the image's built-in firewall
  default of `api.anthropic.com`):
  ```sh
  -e ANTHROPIC_API_KEY
  ```
  (or `-e CLAUDE_CODE_OAUTH_TOKEN`, from `claude setup-token`)
- **Google Vertex AI** (this deployment's actual auth path — needs
  `CHORUS_SANDBOX_ALLOW_HOSTS` to add Vertex's endpoint, since it isn't
  in the built-in default):
  ```sh
  -e CLAUDE_CODE_USE_VERTEX=1 \
  -e CLOUD_ML_REGION=<region> \
  -e ANTHROPIC_VERTEX_PROJECT_ID=<project> \
  -v ~/.config/gcloud:/home/node/.config/gcloud:ro \
  -e CHORUS_SANDBOX_ALLOW_HOSTS=<region>-aiplatform.googleapis.com,oauth2.googleapis.com,accounts.google.com
  ```
  ADC (`gcloud auth application-default login`, run on the host first)
  is what the mounted `~/.config/gcloud` supplies; a mounted
  service-account JSON + `GOOGLE_APPLICATION_CREDENTIALS` works the same
  way instead if that's how your environment issues credentials.
- **Amazon Bedrock**:
  ```sh
  -e CLAUDE_CODE_USE_BEDROCK=1 -e AWS_REGION=<region> -e AWS_ACCESS_KEY_ID=... -e AWS_SECRET_ACCESS_KEY=... \
  -e CHORUS_SANDBOX_ALLOW_HOSTS=bedrock-runtime.<region>.amazonaws.com
  ```

Never mount the host's real `~/.claude` (its live interactive-login
state) regardless of which option you use — the image already creates a
fresh, empty `/home/node/.claude` for container-local state only.

## `agents.yaml` snippet (Vertex example, this deployment's real setup)

```yaml
- name: claude
  spawn: ["docker", "run", "--rm", "-i",
          "--cap-add=NET_ADMIN", "--cap-add=NET_RAW",
          "-v", "{{CWD}}:/workspace",
          "-v", "chorus-te-data:/home/node/.tokeneconomizer",
          "-v", "~/.config/gcloud:/home/node/.config/gcloud:ro",
          "-e", "CLAUDE_CODE_USE_VERTEX=1",
          "-e", "CLOUD_ML_REGION=<region>",
          "-e", "ANTHROPIC_VERTEX_PROJECT_ID=<project>",
          "-e", "CHORUS_SANDBOX_ALLOW_HOSTS=<region>-aiplatform.googleapis.com,oauth2.googleapis.com,accounts.google.com",
          "chorus-claude-sandbox"]
  workdir: /workspace
  cost_tier: metered
```

`{{CWD}}` is substituted by chorus itself (`internal/session.Connect`,
per `session.Spec.WorkDir`/`EffectiveCwd`) with the real host project
directory before this command is exec'd — the `agents.yaml` entry stays
portable across machines/clones instead of hardcoding an absolute path.

The `chorus-te-data` named volume persists the TokenEconomizer plugin's
learned data (`~/.tokeneconomizer`) across runs. It is REQUIRED for the
plugin baked into the image to be worth anything — with `--rm` the
container is discarded every run, and a named volume is the only state
that survives. Drop this `-v` line to disable the plugin's learning
entirely (the plugin stays installed but starts cold every run). The
volume is keyed internally by repo, so one shared volume across all
projects is fine. Once per image rebuild (new Claude Code version),
run `/tokeneconomizer:toolprune refresh-catalog` once from a chorus
session to rebuild its tool catalog.

CAVEAT — remote-less projects collide. The plugin keys a repo by its
git `origin` remote URL, NOT by path, so distinct git repos stay cleanly
separated in the shared volume even though every project mounts at the
same `/workspace` inside the container. But a project with no `origin`
remote (not a git repo, or a local-only one) falls back to the checkout
PATH — which inside the container is always `/workspace` — so ALL such
remote-less projects collide onto one key and cross-contaminate each
other's learned data. Multiple chorus sessions on the SAME repo (same or
different folders) are safe from corruption — the plugin locks
(`fcntl.flock`) and writes atomically — you just get some last-writer-wins
learning churn. If you run several remote-less projects and want them
isolated, give each its own volume (rename `chorus-te-data` per project)
or drop the `-v` line for those.

## Verifying containment

Once running under chorus, confirm:

1. Claude can read/edit files under the real project (mounted at
   `/workspace`).
2. Claude CANNOT read/write anything outside that mount — ask it to
   `cat /etc/passwd` or list `/`; expect a permission failure or empty
   view, never host file contents.
3. It can still reach Vertex (or whichever auth option you configured)
   and complete a real prompt.
4. An attempted connection to an unrelated host (e.g. `curl
   https://example.com` via its own shell tool) is blocked — the
   firewall script itself verifies this same check at container start
   and fails loudly if it doesn't hold.
