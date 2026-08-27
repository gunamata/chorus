# Gemini CLI sandbox image — direct wrap, bypassing the CLI's own sandbox

**Why this exists, and why it's different from Claude/opencode's
images**: Gemini CLI has its own built-in `GEMINI_SANDBOX=docker`
sandboxing, which was the original plan for this agent (see
`sandbox/README.md`) — but it's **confirmed incompatible with `--acp`
mode** (2026-08-26, chorus-spec.md §0): its relaunch-into-sandbox logic
appears to do a blocking read against stdin before attempting the
relaunch. `--acp` mode needs a persistent, never-EOF stdin pipe (exactly
what chorus provides) — the two deadlock. Standalone `gemini --acp` with
a real open pipe hung indefinitely with no container ever created;
`gemini --acp < /dev/null` (immediate EOF) DID spawn a container, one
that exited instantly since there was no real protocol traffic to
process.

The fix: bypass Gemini's own relaunch entirely. This image is built
**on top of Google's own real sandbox image**
(`us-docker.pkg.dev/gemini-code-dev/gemini-cli/sandbox:<version>` —
confirmed live by pulling and inspecting it; NOT
`ghcr.io/google/gemini-cli`, an earlier wrong guess) with our own
firewall layered on, the same pattern as `sandbox/claude/`. chorus
invokes `docker run` directly and owns the stdio piping itself — there
is no relaunch step to deadlock, since the container's `gemini` process
reads its first ACP message straight from the pipe chorus already owns.

## Build

The base image tag **must match your installed `gemini-cli` version**
(`gemini --version`, or check `CLI_VERSION` via `docker inspect` on
whatever image your own `GEMINI_SANDBOX=docker` attempts pulled — see
your `.chorus/logs` or terminal output for the exact tag it tried,
`0.57.0` at the time this was built):

```sh
docker build --build-arg GEMINI_CLI_VERSION=0.57.0 -t chorus-gemini-sandbox sandbox/gemini/
```

## Auth — OAuth ("Sign in with Google") + Code Assist, this deployment's real setup

This is a **different auth path than Vertex AI** (see
`sandbox/claude/README.md`, which uses Vertex) — Gemini CLI's own OAuth
login, which for a Workspace/company account or a Gemini Code Assist
license additionally needs a bound GCP project:

```sh
-e GOOGLE_CLOUD_PROJECT
```
(bare, pulled from chorus's own process environment — set it in the
shell you launch chorus from)

**Credentials**: run `gemini` interactively once, outside any
container, to complete the OAuth browser flow if you haven't already.
Mount the resulting credential directory — **NOT read-only**:

```sh
-v {{ENV:HOME}}/.gemini:/home/node/.gemini
```

**Read-only was tried first and confirmed broken (2026-08-27, live)**:
gemini's `OAuth2Client` tries to write back to `oauth_creds.json` after
loading it (`cacheCredentials` in `@google/gemini-cli-core`), and a
`:ro` mount makes that write fail with `EROFS` — an unhandled promise
rejection visible in gemini's own debug output
(`DEBUG=true`). Switching to a read-write mount was the fix that
actually unblocked the ACP handshake. This is a real, deliberate
tradeoff: a sandboxed Gemini can now modify your real host OAuth
credential file — not a pure filesystem sandbox for this one path, but
the alternative is Gemini never completing authentication at all.

**Known limitation, not yet resolved**: if your OAuth credentials are
stored in your OS's keychain/keyring service rather than the plain
`~/.gemini/oauth_creds.json` file (macOS Keychain, GNOME
libsecret, Windows Credential Manager — see `agents.yaml.sandbox.*`'s
Gemini comment for the research behind this), mounting `~/.gemini`
alone won't carry the actual secret into the container, and gemini will
likely fail to authenticate or attempt a fresh (impossible, headless)
browser login instead. This is most likely to actually work as-is on a
Linux machine with no desktop keyring daemon running, where gemini-cli's
own fallback is the plain JSON file — verify which applies to your
machine before assuming this works.

No `GEMINI_SANDBOX` env var is needed or used by this approach at all —
chorus IS the sandbox invocation now, not Gemini's own internal one.

## `agents.yaml` snippet

```yaml
- name: gemini
  spawn: ["docker", "run", "--rm", "-i", "--cap-add=NET_ADMIN", "--cap-add=NET_RAW",
          "-v", "{{CWD}}:/workspace",
          "-v", "{{ENV:HOME}}/.gemini:/home/node/.gemini",
          "-e", "GOOGLE_CLOUD_PROJECT",
          "-e", "CHORUS_SANDBOX_ALLOW_HOSTS",
          "chorus-gemini-sandbox", "--acp"]
  workdir: /workspace
  cost_tier: seat
```

## Firewall allowlist

Base default covers GitHub, npm, and the three Google endpoints this
OAuth+Code-Assist auth path needs (confirmed via gemini-cli's own GitHub
issues, not guessed): `oauth2.googleapis.com`, `accounts.google.com`,
`cloudcode-pa.googleapis.com` (the actual Code Assist API endpoint for
OAuth-authenticated requests — NOT `generativelanguage.googleapis.com`,
which is the separate direct-API-key endpoint this auth path doesn't
use). If you switch this agent to Vertex AI auth instead, extend via
`CHORUS_SANDBOX_ALLOW_HOSTS` the same way Claude's Vertex entry does.

## Verifying containment

Same checklist as `sandbox/claude/README.md`: workspace mount works,
nothing outside it is reachable, `curl https://example.com` (or any
unrelated host) is blocked, and — the thing this whole image exists to
fix — a real ACP `initialize` handshake through chorus actually
completes instead of hanging.

**All of the above CONFIRMED LIVE end-to-end (2026-08-27)**: with the
read-write `.gemini` mount, `./chorus --agents=agents.yaml.sandbox.linux`
completed real ACP handshakes for all three sandboxed agents (Claude,
Gemini, opencode) — `commands` showed each agent's real advertised
slash-command list, confirming Gemini's `initialize` genuinely
completed, not just that a container started. This was the last unknown
in the whole sandboxing feature; see `chorus-spec.md` §0's 2026-08-27
entries for the full diagnostic trail (blocking-stdin-read deadlock in
Gemini's own relaunch mechanism, the EROFS credential-write bug, and how
each was actually found and fixed, not guessed).
