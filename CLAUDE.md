# CLAUDE.md

Guidance for Claude Code sessions working in this repo.

## What this is

chorus: a Go CLI that owns several ACP (Agent Client Protocol)
subprocess sessions — Claude Code, Gemini CLI, and opencode — and lets
a user drive all of them from one terminal. User-facing docs live in
`README.md` — keep it in sync with the code whenever you change
behavior it describes (usage syntax, `agents.yaml` shape, prerequisites,
known limitations).

## Build / run / test

```sh
go build .          # ./chorus (macOS/Linux) or chorus.exe (Windows)
go vet ./...
gofmt -l .           # should print nothing; gofmt -w to fix
go test ./...
./chorus             # run (spawns all agent subprocesses)
```

**Releases**: `.github/workflows/release.yml`, triggered by a `v*.*.*`
tag push, cross-compiles `linux`/`darwin`/`windows` × `amd64`/`arm64`
binaries from one Linux runner (no cgo anywhere in the module graph, so
this is safe), publishes them plus `agents.yaml` and `checksums.txt` to
a GitHub Release, and builds/pushes the `sandbox/` Dockerfiles to Docker
Hub as `matamagu/chorus-<agent>-sandbox` (Claude/opencode for
`linux/amd64`+`linux/arm64`; Gemini `linux/amd64` only, since its base
image's `arm64` availability was never confirmed). Needs
`DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN` (an access token, not the account
password) as repo secrets for the Docker half. `main.go`'s
`version`/`commit`/`date` vars are stamped via `-ldflags` in that
workflow — `chorus --version` surfaces them. The `softprops/action-gh-release`
step sets `prerelease: false`/`make_latest: true` explicitly — don't
remove these. Early releases were manually marked "prerelease" on
GitHub after creation, which made them invisible to
`/releases/latest` (GitHub excludes prereleases/drafts from that
endpoint by design) even though the assets themselves were complete —
the exact failure a user hit live via `install.sh`/`install.ps1`'s
"look up the latest release" step. Explicit here so a normal release
never depends on remembering not to check that box, and so a
`workflow_dispatch` re-run against an existing release is
deterministic — this action's documented behavior is that an unset
field on a re-run RETAINS whatever the existing release already had.

`install.sh` (POSIX sh)/`install.ps1` (Windows) download the matching
release binary and seed `~/.chorus/agents.yaml` on first install only
(never overwritten after, so local edits survive an upgrade). Env
overrides: `CHORUS_VERSION`, `CHORUS_INSTALL_DIR`, `CHORUS_HOME`,
`CHORUS_AGENTS=<path-or-https-url>` (seeds a custom config instead of
the bundled default — same local-file-or-URL support as chorus's own
`--agents` flag, same `http://`-rejection reasoning: the seeded file is
what chorus execs `spawn` commands from unconditionally, so a plaintext
fetch is a real command-injection vector). Gotchas worth preserving if
you touch these scripts:
- `sha256sum`'s own output format varies (plain space vs.
  space-then-asterisk before the filename) — match by filename/awk-field,
  never assume exact spacing.
- `install.ps1` must stay plain ASCII. Windows PowerShell 5.1 doesn't
  assume UTF-8 for a script with no byte-order mark — it falls back to
  the system codepage, which can misdecode a multi-byte character (an
  em-dash, say) and corrupt a string literal's boundary, desyncing the
  parser in a way that produces wildly misleading errors elsewhere in
  the file. POSIX shells don't have this problem, so `install.sh` isn't
  affected the same way.
- In docs, an env var meant for a piped script belongs on the command
  that CONSUMES the pipe, not the one that produces it: `curl ... | VAR=val sh`,
  never `VAR=val curl ... | sh` (the latter sets it for `curl`, and it
  never reaches `sh` at all).

**Test coverage**: `internal/render`, `internal/policy`, `internal/router`,
`internal/registry`, `internal/sessionstore`, `internal/acpclient`,
`internal/tui`, and package `main` all have real unit tests — add tests
here first when changing behavior in these packages.
`internal/acpclient`'s permission-gating tests (`checkPermission` and
the three client-owned RPCs it gates) are the ONLY way to verify that
logic at all: live testing confirmed neither Claude nor opencode
currently invoke `ReadTextFile`/`WriteTextFile`/`CreateTerminal` (both
do file I/O and shell execution inside their own process), so a live
smoke test would never exercise this code regardless of thoroughness.
`internal/session` and most of `internal/delegate` remain untested by
plain `go test` — they need a live subprocess or network mocking to
exercise meaningfully (`internal/delegate/roster.go` is the exception:
pure logic, has real coverage). `internal/headroom` also has two
go:build-tag-gated LIVE tests, excluded from the normal `go test ./...`
run the same way, but runnable when the prerequisite is actually
available: `internal/headroom/live_test.go` (`-tags headroom_live`,
needs a real docker daemon) exercises the real Start/Stop against the
real published image; `internal/session/live_headroom_test.go` (`-tags
claude_headroom_live`, needs docker AND an authenticated `claude` CLI
subscription) goes further and drives a real `claude-agent-acp`
subprocess through a real Headroom container with a real prompt — this
pair is what caught the docker-run command bug and the `slim` variant
crash documented in internal/headroom's own entry below; keep both
current if you touch that package's docker-invoking code, since
stubbed-runDocker unit tests structurally can't catch that class of bug.

`chorus __mcp_delegate` isn't meant to be run directly — it expects
`CHORUS_DELEGATE_ADDR`/`_TOKEN`/`_SOURCE` env vars that only `main.go`
sets when spawning it through an agent's `mcpServers` config.

**Windows gotcha:** if a built binary suddenly fails to execute with no
compiler error ("Application Control policy has blocked this file" —
confirm via `Get-WinEvent -LogName Microsoft-Windows-CodeIntegrity/Operational`),
that's Windows Smart App Control blocking an unsigned local binary, not
a code regression. Fixing it (disabling Smart App Control) is a one-way
toggle and the user's call, not something to do unilaterally.

## Architecture

```
main.go               Startup only — no long-lived loop lives here.
                      Phases 1-3 (agent connect, delegate hub, session
                      create/resume, banners) run as plain fmt.Print* to
                      stdout, finishing BEFORE internal/tui exists. Also
                      handles the `chorus __mcp_delegate` subcommand in
                      main() itself, before run() starts.
                      loadAgentConfig resolves agents.yaml in order:
                      (1) --agents=<path-or-url> — hard error if
                      missing/unfetchable; an http(s):// value goes
                      through fetchAgentsYAML instead of os.ReadFile
                      (re-fetched fresh every run, capped at 1 MiB/15s,
                      http:// rejected outright since the fetched file
                      is exec'd unconditionally); (2) a local
                      ./agents.yaml in cwd; (3) a central
                      ~/.chorus/agents.yaml (sessionstore.HomeDir,
                      CHORUS_HOME-overridable — what install.sh/
                      install.ps1 seed); (4) the embedded default
                      (//go:embed agents.yaml — this repo's own
                      reference config, also what the release workflow
                      publishes for step 3's seeding, so there's one
                      source of truth, not two copies that could
                      drift). Only step 1 is a hard error on failure;
                      2-4 are a plain progressive fallback.
                      Phase 3 also seeds a one-time delegation briefing
                      (buildBriefingText) once 2+ agents are connected,
                      gated on sessionstore.Store.Briefed (independent
                      of session-resume status — a session that existed
                      before a 2nd agent became available still gets
                      briefed on its next run) unless
                      delegation.briefing is false. Queued via
                      tui.QueuePrompt as a real visible turn.
                      Once phase 3 finishes, main.go's entire remaining
                      job is `tea.NewProgram(tui.New(cfg), opts...).Run()`.
                      opts is WithAltScreen() plus, unless
                      CHORUS_DISABLE_MOUSE is set, WithMouseCellMotion().
                      Also forces glamour's markdown style once via
                      lipgloss.HasDarkBackground() STRICTLY BEFORE
                      tea.NewProgram runs — glamour's own auto-detection
                      queries the terminal over stdin/stdout, which
                      could otherwise race against bubbletea taking over
                      raw stdin at the same moment.

internal/tui          Bubbletea Model/Update/View. Model holds five
                      channels (outputCh/permCh/errCh/doneCh/
                      delegateLogCh), *render.Renderer,
                      *delegate.Collectors, workers, routing config,
                      commands map, a bubbles/viewport.Model (scrolling
                      output), a bubbles/textarea.Model (multi-line
                      input — ctrl+j for a newline, Enter submits), and
                      an ordered []block document: a block with a
                      non-empty key overwrites the current LAST block if
                      it shares that key, else appends.
                      One blocking-receive tea.Cmd per channel, re-armed
                      after each message EXCEPT waitForPermission, which
                      is only re-armed once pendingPerm returns to nil
                      (reproduces FIFO permission queuing — see
                      concurrency invariant #4). helpers.go holds pure
                      helpers relocated from main.go (dispatch,
                      queuePrompt, AgentWorker/StartWorker, formatters).
                      syncViewport() re-renders the whole document on
                      every change; auto-follow-scroll unless a
                      permission/routeAsk menu is pending, in which case
                      it always forces scroll-to-bottom (a menu must
                      never be scrollable out of view). renderDocument()
                      word-wraps the whole document (muesli/reflow/wordwrap,
                      ANSI-safe) since bubbles/viewport TRUNCATES rather
                      than wraps overflowing lines — this only breaks on
                      spaces and `-`, so a single long unbroken token (a
                      long path, URL, or hash) still won't wrap and can
                      get clipped on a narrow terminal; a known,
                      deliberate tradeoff, not fixed (broadening
                      breakpoints to `/`/`\` would also make normal
                      prose like "and/or" breakable).
                      "!<cmd>" native commands (handleNativeCommand)
                      bypass dispatch/routing entirely — exec via `cmd /C`
                      (Windows) or `sh -c` (else) so pipes/chains work,
                      no permission gate (the human typed it themselves,
                      same trust level as a plain terminal), output
                      capped at 200KB.
                      Esc (modeNormal only) calls interruptBusyAgents →
                      interruptAgents(selectInterruptTargets(...)) →
                      AgentWorker.Cancel → ACP's session/cancel,
                      targeting lastRoutedAgent if it's busy else every
                      busy agent. ACP doesn't guarantee an early stop,
                      only that the notification was sent.
                      Ctrl+C (2026-09) shares interruptAgents but skips
                      the targeting narrowing entirely: `if busy :=
                      m.busyAgentNames(); len(busy) > 0` wins outright,
                      before input content is even read — interrupts
                      EVERY busy agent, leaves the input box untouched,
                      and only falls through to the old clear-draft-then-
                      quit logic once nothing is running. Two keys, two
                      deliberately different scopes: Esc narrows to what
                      you're likely watching, Ctrl+C is the blunt
                      stop-everything one. Untestable end-to-end via
                      plain `go test` for the same reason the rest of
                      AgentWorker.Cancel's callers are (needs a live
                      session.AgentSession/ACP connection) —
                      TestModel_CtrlC_TakesInterruptBranchWhenBusy proves
                      the branch is taken anyway, by asserting on the
                      resulting panic from a deliberately nil-session
                      test double rather than working around it.
                      Click-drag (mouse capture on by default) does
                      LINE-level (not character-column) text selection +
                      copy-on-select via github.com/atotto/clipboard,
                      highlighted live via renderer.HighlightLine
                      (persists until the next keypress). Deliberately
                      no OSC 52 fallback for SSH — that would mean
                      writing raw bytes to os.Stdout outside bubbletea's
                      render cycle (see concurrency invariant #3).
                      `tea.WithMouseCellMotion()` is kept on despite
                      taking over plain click-drag from the terminal's
                      own selection — most terminal emulators let a
                      modifier key (commonly Shift) override a program's
                      mouse capture and select natively anyway.
                      `CHORUS_DISABLE_MOUSE` is the escape hatch for a
                      terminal where that doesn't work; don't remove
                      `WithMouseCellMotion()` itself without live
                      evidence the override genuinely fails somewhere.
                      `modes`/`mode <agent> <id-or-name>`/`auto [agent]`
                      (ACP session modes — e.g. Claude Code's own "accept
                      edits"/"bypass permissions" concept) run
                      session/set_mode asynchronously via runSetMode (a
                      tea.Cmd, same async one-shot pattern as
                      runNativeCommand — never call SetMode directly from
                      Update, same reasoning as invariant #1).
                      `auto`/`auto <agent>` toggles into agents.yaml's
                      per-agent `auto_mode` and back, remembering the
                      prior mode in Model.autoPrevMode so a second call
                      restores it; chorus never guesses a mode string —
                      `auto_mode` is something you discover via `modes`
                      and set yourself.
                      handleKey wraps handleKeyDispatch: refreshSuggest
                      (recomputes the live "/"/"@" popup from input
                      content) and relayout (resizes the viewport for the
                      popup's line count) run unconditionally after EVERY
                      keypress, regardless of which branch fired — don't
                      scatter these calls into individual
                      handleKeyDispatch branches instead. relayout is the
                      single source of viewport-sizing math (handleResize
                      and handleKey's wrapper both call it), so a popup
                      opening/closing mid-typing resizes the viewport
                      exactly like a real terminal resize does.
                      A literal Ctrl+V keystroke only ever reaches the
                      program when the terminal had no text to
                      bracket-paste (an image-only or empty clipboard) —
                      real terminals intercept a text-bearing Ctrl+V as
                      bracketed paste first. See readClipboardImage
                      (internal/tui/clipboardimage*.go) and "Known
                      limitations" for per-OS coverage.

internal/session      Connection (one subprocess + ACP initialize
                      handshake — records SupportsLoadSession,
                      SupportsImagePrompts) and AgentSession (one
                      session on a Connection — the main interactive
                      session, or a short-lived delegation sub-session).
                      Split this way so delegation sub-sessions reuse an
                      already-initialized subprocess instead of spawning
                      a new one per delegated call.
                      AgentSession.Prompt/PromptContent BLOCK for one
                      whole turn — never call either from internal/tui's
                      Update loop (see concurrency invariant #1).
                      Connect's stderr param is never os.Stderr — a
                      per-agent file under the project state dir's
                      logs/<agent>.stderr.log instead (or io.Discard),
                      so a subprocess's raw stderr can never land on the
                      same screen bubbletea is actively redrawing.
                      AgentSession also holds ACP session-mode state
                      (AvailableModes/CurrentModeId, from
                      NewSession/LoadSession's response) and SetMode
                      (session/set_mode) — backs the `modes`/`mode`/`auto`
                      REPL commands (internal/tui). SetMode doesn't update
                      CurrentModeId itself; the agent's own
                      current_mode_update notification is the source of
                      truth (internal/tui applies it belt-and-suspenders
                      after a successful SetMode too, in case an agent
                      doesn't send one).

internal/acpclient    Implements acp.Client — the callback interface the
                      SDK invokes from the subprocess's own read
                      goroutine. SessionUpdate -> outputCh.
                      RequestPermission -> policy check (Kind- and
                      title-based) -> permCh, blocks on a per-request
                      response channel. ReadTextFile/WriteTextFile/
                      CreateTerminal go through the same checkPermission
                      gate (see security invariant #1). WriteTextFile/
                      ReadTextFile reject UNC paths before any
                      permission check (security invariant #2).
                      CreateTerminal honors OutputByteLimit.

internal/render       Pure formatting: bus.Update -> (key, text, ok) via
                      FormatUpdate/FormatSpinnerTick — no I/O of its
                      own. StripANSI (also used by internal/tui) removes
                      terminal escape sequences from agent-supplied text
                      before it's formatted (security invariant #3).
                      ShowThoughts defaults false — an agent_thought_chunk
                      renders a brief rotating-word indicator instead of
                      full text (the `thoughts` REPL command toggles it
                      on). available_commands_update renders a terse
                      one-liner ("commands available — type \"commands\"
                      to list"), not the full list — a large or
                      repeatedly-reported command set turned into
                      multi-line noise otherwise. Tool-call text content
                      is capped at toolCallContentPreviewLimit (500
                      chars, rune-boundary-safe) — opencode's ACP
                      adapter includes a read tool's ENTIRE file content
                      in ToolCallContent, which with no cap dumped whole
                      files into the scrollback; the cap is
                      agent-agnostic, not an opencode special case.

internal/policy       Types + matching logic only — doesn't parse
                      agents.yaml itself (internal/registry.Parse does,
                      the single decode point for the whole file).
                      AutoAllow is Kind-based (ACP's ToolCallUpdate.Kind
                      — read/edit/delete/move/search/execute/think/
                      fetch/switch_mode/other — the one thing every
                      agent reports uniformly; per-agent tool names
                      aren't visible over ACP at all).
                      AutoAllowTools matches by TITLE PREFIX, not
                      substring (security invariant #4) — needed for
                      chorus's own `delegate` tool, whose Kind is
                      generic "other" to every agent.
                      Delegation.Enabled defaults FALSE (opt-in — see
                      "Cross-agent delegation" below). Routing is
                      {Mode, DecisionAgent, ContextLevel} (off/llm).

internal/router       LLM-based routing decision, pure logic:
                      BuildDecisionPrompt (self-identifying framing +
                      each candidate agent's cost_tier/notes/models +
                      the user's prompt, asking for a specific JSON
                      reply) and ParseDecision (brace-matches the first
                      {...} object — tolerant of a ```json fence or
                      leading prose, validates agent/model against the
                      known set, returns a plain error on anything
                      unparseable rather than panicking). No ACP
                      structured-output primitive exists for a normal
                      prompt turn, so this is inherently best-effort;
                      internal/tui always falls back to default_agent
                      on any ParseDecision error rather than blocking
                      the user's real prompt.

internal/registry     Parse(): agents.yaml -> ([]session.Spec,
                      policy.Config), in file order (a YAML map would
                      decode into a Go map with no iteration order,
                      making agent startup nondeterministic — this is
                      why agents.yaml is a top-level LIST, not a map
                      keyed by name; don't change that). Spec.CostTier/
                      Notes/Models are threaded through for
                      internal/delegate (roster/briefing) and
                      internal/router (candidate-agent list) — unused
                      by registry/session themselves. Spec.AutoMode
                      (yaml `auto_mode`) is optional per agent, backs the
                      `auto` REPL command — see internal/session's entry.
                      Spec.Env (yaml per-agent `env:`) is optional extra
                      environment for a bare host-process spawn only —
                      see internal/headroom's entry for the motivating
                      case and why it's a no-op for a sandboxed `docker
                      run` spawn.

internal/headroom     Optional chorus-managed Docker container running
                      Headroom (https://docs.headroomlabs.ai/docs), a
                      local compression proxy that sits between an agent
                      CLI and its LLM provider. defaultImage is
                      `code-nonroot`, deliberately not `latest` —
                      Headroom's tags are an 8-way matrix of the `code`/
                      `slim`/`nonroot` build modifiers (`latest` has
                      none of them); `code` enables AST-aware code
                      compression (the actually-relevant capability,
                      since these agents' tool output is overwhelmingly
                      source/diffs), `nonroot` runs as uid 1000. `slim`
                      is deliberately EXCLUDED — CONFIRMED LIVE (2026-09,
                      real docker run on Rancher Desktop/Windows, WSL2
                      linux/amd64 backend): every `slim`-tagged variant
                      segfaults on startup (exit 139, zero log output),
                      isolated to the distroless base itself (both
                      code-slim-nonroot AND plain slim-nonroot crashed
                      identically; code-nonroot, non-slim, started clean
                      and served real compressed requests in the same
                      test). May be environment-specific — re-test before
                      reconsidering, don't just revert on the reasoning
                      alone. See defaultImage's own doc comment for the
                      full account. agents.yaml's top-level
                      `headroom:` block (Config, EnabledOrDefault false)
                      — Start(ctx, cfg) targets a FIXED name/volume
                      (containerName "chorus-headroom", volumeName
                      "chorus-headroom-data" — not per-run random ones;
                      2026-09 redesign, see this file's own git history
                      if you need the old ephemeral-per-run shape) and is
                      idempotent: `docker inspect` first — already
                      running -> reused as-is, no docker command needed;
                      exists but stopped -> `docker start` (preserves its
                      original --restart/volume/port); never existed ->
                      `docker run -d --name chorus-headroom --restart
                      unless-stopped -v chorus-headroom-data:/home/nonroot/.headroom
                      -p {port}:8787 -e HEADROOM_HOST=0.0.0.0 -e
                      HEADROOM_PORT=8787 -e HEADROOM_MODE=... {image}`
                      with NO trailing command — CONFIRMED LIVE (2026-09)
                      that appending one (an earlier version did:
                      "headroom proxy --host 0.0.0.0 --port 8787") breaks
                      the container outright: the image's own ENTRYPOINT
                      is already `python3 -m headroom.cli proxy` with a
                      complete default CMD, so extra positional args
                      after the image name are read as unexpected
                      arguments to that already-complete command and the
                      container exits immediately ("Error: Got unexpected
                      extra arguments"). No stubbed-runDocker unit test
                      could have caught this — see
                      internal/session/live_headroom_test.go and
                      internal/headroom/live_test.go (both
                      go:build-tag-gated, excluded from the normal `go
                      test ./...` run, real docker/real Claude required)
                      for the tests that did. Then polls /stats until it
                      responds (startTimeout 30s), returns a *Proxy. On a
                      health-check failure, the container is only
                      removed if THIS call freshly created it via `docker
                      run` — a reused (already-existing) container is
                      left alone, since chorus didn't create it this run
                      and a possibly-transient timeout doesn't justify
                      destroying someone else's container. Stop() (plain
                      `docker rm -f`) still exists but is NOT called
                      anywhere in main.go's normal run path — the whole
                      point of this redesign (--restart unless-stopped +
                      a named volume) is a container that outlives any
                      one chorus session and keeps its provider-side
                      prompt cache warm across runs; only tests/manual
                      cleanup call Stop() now. docker invocation is
                      behind the runDocker function var (same stubbing
                      pattern as internal/tui's writeClipboard/
                      readClipboardImage) so Start/Stop's argument-
                      building is unit-tested without a real docker
                      daemon (the tests stub `docker inspect`'s output
                      too, to exercise all three reuse/start/create
                      paths). main.go's phase 0 (before phase 1's agent
                      connects) calls Start, THEN os.Setenv's
                      CHORUS_HEADROOM_HOST_URL/_SANDBOX_URL — order
                      matters, since both {{ENV:...}} in spawn Args and
                      the new Spec.Env are resolved at spawn time, not
                      parse time. Proxy.HostURL()
                      (http://127.0.0.1:{port}) is for a NON-sandboxed
                      agent; Proxy.SandboxURL()
                      (http://host.docker.internal:{port}) is for a
                      SANDBOXED agent's own container — chorus never
                      guesses which one a given agents.yaml entry needs,
                      let alone which provider-specific base-URL env var
                      name (ANTHROPIC_BASE_URL etc.) an agent's CLI
                      honors; the agents.yaml author wires both
                      explicitly (README has worked examples). The
                      container's port is published on EVERY host
                      interface, not 127.0.0.1-only — see Start's own doc
                      comment and README's security note for why
                      (Linux Rancher Desktop's containers reach the host
                      over a real docker bridge interface that does NOT
                      reach a loopback-only-bound port, unlike Docker
                      Desktop's macOS/Windows VM-proxied networking).
                      Provider API keys (ANTHROPIC_API_KEY/OPENAI_API_KEY/
                      AWS/GOOGLE_APPLICATION_CREDENTIALS) are passed to
                      the container as bare `-e NAME` (pulls from
                      chorus's own process env if set, omitted if not —
                      same convention agents.yaml.sandbox.* already uses)
                      but are NOT required for the core proxy-passthrough
                      case — see "Known limitations" for what's actually
                      unverified here.

internal/delegate     Cross-agent delegation. Two roles: RunMCPServer
                      (the `chorus __mcp_delegate` subcommand — an MCP
                      stdio server exposing one `delegate` tool, spawned
                      by an AGENT subprocess, not chorus) and Hub
                      (chorus's own loopback-only HTTP server —
                      127.0.0.1, random per-run port and token,
                      constant-time comparison — security invariant #5)
                      that RunMCPServer calls back into, since it's a
                      separate OS process with no shared memory.
                      Collectors bridges Hub's synchronous "wait for the
                      sub-session's reply" need with main's single
                      outputCh consumer, keyed by SessionId (concurrency
                      invariant #5). roster.go is the single source of
                      truth for "which other agents exist, at what cost
                      tier" — feeds both the delegate tool's description
                      and the seeded briefing from one call, not two
                      descriptions that could drift.
                      This is the one deliberate exception to "no
                      sockets" in this codebase — required because ACP's
                      mcpServers mechanism spawns the delegate server
                      from the agent subprocess, not from chorus, so a
                      loopback callback is the only way for it to reach
                      back into chorus's main process at all. Don't let
                      it grow into a general-purpose IPC mechanism for
                      anything else.

internal/sessionstore sessions.json persistence: agent name -> last
                      main-session ID, keyed per-project (an ACP session
                      ID is only valid for the cwd it was created in).
                      Only main sessions are ever stored — delegation
                      sub-sessions never touch this.
                      Lives centrally under ~/.chorus/projects/<slug>/
                      (ProjectDir — HomeDir, overridable via CHORUS_HOME,
                      joined with cwd's base name plus a short hash of
                      the full absolute path, case-folded on Windows so
                      two different-case references to the same path
                      collide onto the same slug), not a per-project
                      ./.chorus/. main.go computes ProjectDir(cwd) once
                      at startup and joins "sessions.json"/"logs"/
                      "images" onto it.
                      HomeDir (the un-keyed root itself) is also reused
                      directly by main.go's loadAgentConfig for the
                      central agents.yaml tier — one shared root/override
                      for both "where does this project's session state
                      live" and "where does the shared default config
                      live."

internal/bus          Shared message types (Update, PermissionRequest)
                      so acpclient and render don't import each other.

sandbox/              Opt-in per-agent container images for filesystem/
                      network containment — see sandbox/README.md and
                      the per-agent READMEs for usage. Requires almost
                      no chorus code: agents.yaml's `spawn` is already
                      an arbitrary command line, so a sandboxed entry is
                      just `spawn: ["docker", "run", ...]` instead of
                      the bare CLI.
                      session.Spec.WorkDir + Spec.EffectiveCwd(hostCwd)
                      (empty WorkDir = unchanged non-sandboxed behavior)
                      plus a literal `{{CWD}}` token in spec.Args,
                      substituted with the real host cwd by
                      session.Connect, solve the cwd-translation problem
                      (a container can't resolve the real host path —
                      its filesystem view is only its mount point).
                      `{{ENV:NAME}}` similarly substitutes os.Getenv(NAME)
                      for machine-specific paths (e.g. a mounted gcloud
                      config directory) without hardcoding a user's name
                      into agents.yaml.
                      Per-agent approach deliberately differs, since the
                      best available mechanism differs per agent:
                      sandbox/claude/ adapts Anthropic's own official
                      devcontainer directly. sandbox/gemini/ builds
                      directly on Google's own real sandbox image
                      (us-docker.pkg.dev/gemini-code-dev/gemini-cli/sandbox)
                      rather than using Gemini CLI's own
                      GEMINI_SANDBOX=docker mechanism, which is
                      CONFIRMED INCOMPATIBLE with --acp mode (its
                      relaunch-into-sandbox logic does a blocking stdin
                      read that deadlocks against --acp's required
                      persistent, never-EOF pipe). sandbox/opencode/ is
                      fully custom (no official image exists for
                      opencode at all).
                      Auth and the firewall's egress allowlist are never
                      hardcoded into an image — both are runtime
                      `docker run -e/-v` configuration, with
                      CHORUS_SANDBOX_ALLOW_HOSTS extending each image's
                      firewall allowlist per-deployment without a
                      rebuild. Cross-agent delegation is NOT supported
                      for a sandboxed agent (its delegate-mcp process
                      can't reach chorus's loopback Hub from inside the
                      container without extra plumbing) — a sandboxed
                      agent's own entry works standalone.
                      Confirmed live end-to-end for all three agents,
                      including opencode's VPN-bound Ollama backend
                      (not just its free-tier default).
```

## Concurrency invariants — read before touching main.go, session.go, or internal/tui

1. **`AgentSession.Prompt`/`PromptContent` blocks until the whole turn
   completes.** Session updates for that turn stream in concurrently via
   a *different* goroutine (the SDK's own per-connection read loop,
   which calls back into `acpclient.Client`). Never call either
   directly from `internal/tui`'s `Update`/`View` — it would freeze the
   entire bubbletea event loop (rendering AND permission-answering for
   every agent) while that one turn is in flight. Prompts are queued
   through a per-agent `tui.AgentWorker` goroutine (`tui.StartWorker`)
   instead — `Model.Update` only ever sends to `w.in` (via
   `queuePrompt`), never blocks on the RPC itself.

2. **bubbletea owns stdin entirely once `tea.NewProgram(...).Run()`
   starts** (raw mode, its own input-reading goroutine internally) —
   `internal/tui` never reads `os.Stdin` directly, and must not:
   `tea.KeyMsg`/`tea.MouseMsg` arrive through `Model.Update` like any
   other message. Before `Run()` starts, nothing reads stdin at all.

3. **Only `internal/tui`'s bubbletea `Program` writes to the terminal**
   (via `View()`'s returned string). `render.Renderer` methods are pure
   formatters with no I/O of their own, called exclusively from
   `Model.Update`. If you add a new source of user-facing output, route
   it through one of `Model`'s existing channels (or add a new one,
   following the same `waitForX`/re-arm pattern) rather than printing
   directly from another goroutine — a stray `fmt.Print*` from outside
   bubbletea's own render cycle will corrupt the alt-screen display.
   This applies to **agent subprocesses too**: `session.Connect`'s
   subprocess must never inherit `os.Stderr` directly (an agent CLI
   writing an unexpected line to its own stderr would land raw on the
   same screen bubbletea is actively redrawing) — redirect to a
   per-agent log file instead, `io.Discard` on failure, never
   `os.Stderr` as a fallback. Give any new subprocess spawn path the
   same treatment.

4. **`permCh` serializes permission Q&A across all agents** via
   `internal/tui`'s `waitForPermission` re-arm discipline: the returned
   `tea.Cmd` for `permCh` is issued in `Init()` and then *only*
   re-issued from the code path that resolves `Model.pendingPerm` back
   to `nil` (`handlePermissionAnswer`'s success case). While a
   permission is pending, nothing reads `permCh`, so further requests
   queue in its buffer (cap 4) in arrival order. Don't restructure this
   into a manual queue struct — the channel already provides the
   ordering, and re-arming eagerly would let a second request's answer
   race ahead of the first's.

5. **Delegation sub-sessions share their Connection's acpclient.Client
   and outputCh with the main interactive session.** There's no
   separate channel per session — `internal/delegate.Collectors` is
   what lets `Model.handleOutput` tell "this update belongs to a
   delegation sub-session, collect it and hide it" from "this is
   normal interactive output, render it," keyed by `SessionId`. If you
   add another code path that creates sessions, it needs the same
   registration or its output will render inline unexpectedly.

6. **`mcpServers` must never be a nil slice passed straight to the
   wire**, on either `NewSession` or `LoadSession`. It marshals to JSON
   `null`; Claude and Gemini tolerate that, opencode's schema validation
   rejects it outright (`-32602 Invalid params`). Both methods already
   route through the shared `nonNil` helper in `session.go` — don't
   reintroduce a raw pass-through elsewhere.

7. **A saved session ID is not guaranteed to still be resumable** —
   observed live: Claude's agent returned `session/load: Resource not
   found` for an ID that had worked earlier. `resumeOrNewSession`
   (`main.go`) already falls back to a fresh session and clears the
   dead ID on any `LoadSession` error; don't remove that fallback or
   let a resume failure become fatal to startup.

8. **An agent's slash commands aren't known until it's reported them.**
   `Model.commands[agent]` only gets populated when an
   `available_commands_update` actually arrives — observed to sometimes
   only happen after that agent's first turn, not automatically at
   connect/resume. Zero owners found for a real command isn't
   necessarily a bug; it may just be too early.

9. **`AgentWorker.in` is `chan []acp.ContentBlock`, not `chan string`.**
   Every call site goes through `buildPromptBlocks`/`queuePrompt` (or
   the exported `tui.QueuePrompt` for main.go's pre-Model startup use).
   Constructing a `TextBlock` and sending to `w.in` directly means
   `@file.png` attachments silently won't work from that path.

10. **`Model.mode()` is derived, never stored** — it reads
    `pendingPerm`/`pendingRoute` fresh every call rather than caching
    which one is "active." The two fields are deliberately independent
    (not one enum) because a permission request can interrupt and
    display over an outstanding routeAsk WITHOUT discarding it — the
    routeAsk resumes once the permission is answered. Collapsing to one
    enum would silently lose the resume behavior.

11. **Arrow-key menu updates (`permMenuIndex`/`routeMenuIndex`) bypass
    `mergeBlock` on purpose — don't "simplify" them back to it.**
    `mergeBlock` only merges with the current *last* block; a
    permission/routeAsk menu can stay pending for a while with other
    agents' unrelated output streaming in around it, so by the time an
    arrow key arrives the menu is often no longer last. `moveMenuCursor`
    instead writes directly to `m.blocks[permMenuIndex]`/
    `m.blocks[routeMenuIndex]`, tracked at the moment each menu was
    created. `permCursor`/`routeCursor` are two independent fields, not
    one shared cursor — see invariant #10's interrupt-and-resume nuance.

## Security invariants — read before touching acpclient.go, render.go, or internal/tui

1. **Every client-owned RPC that reads/writes a file or runs a command
   must go through `checkPermission` first.** `ReadTextFile`,
   `WriteTextFile`, and `CreateTerminal` in `internal/acpclient` all
   call it before doing anything. If you add a new client-owned RPC
   with real-world side effects, gate it the same way — these three
   executed unconditionally once before an audit caught it, and the
   whole point of `agents.yaml`'s `edit`/`execute` ask-by-default is
   that *nothing* bypasses it.

2. **UNC paths (`\\host\...`, `//host/...`) are rejected outright in
   `ReadTextFile`/`WriteTextFile`, before the permission check, not
   after.** This is a hard boundary, not a policy decision — Windows
   auto-authenticates network paths with the current user's NTLM
   credentials just from the access attempt, so even asking "allow
   this?" is too late. Don't move `isUNCPath` after `checkPermission`,
   and don't make it configurable.

3. **Agent-supplied text gets `render.StripANSI`'d before it's printed,
   at every site that interpolates it into terminal output** (message/
   thought text, diff content, plan entries, tool titles, permission
   option names, command names/descriptions). If you add a new place
   that prints an agent- or tool-supplied string, sanitize it the same
   way — never apply `StripANSI` to chorus's own color constants, only
   to text that originated from an agent. Anything going through `%q`
   is already safe (Go's quoted-string verb escapes control characters).

4. **`Policy.AutoAllowTool` matches by prefix, not substring — don't
   revert this.** A substring check lets any tool call whose
   agent-supplied title merely *mentions* an allowed word bypass its
   real permission requirement (e.g. an `execute`-kind Bash call titled
   "please delegate this"). The shipped default
   (`mcp__chorus-delegate__`) relies on prefix semantics specifically.

5. **`internal/delegate/hub.go`'s token check must stay constant-time**
   (`crypto/subtle.ConstantTimeCompare`, not `==`/`!=`). Also don't
   remove the concurrency semaphore, request size limits, or
   `http.Server` timeouts added alongside it — none of them are
   decorative.

6. **chorus's own state files use owner-only permissions (0o600/0o700)**
   — `sessions.json`, `images/*` under `~/.chorus/projects/<slug>/`.
   This is deliberately *not* applied to `WriteTextFile`'s writes (still
   0o644) — those are the user's own project files an agent edits on
   their behalf, where standard permissions are correct. Don't conflate
   the two when adding new chorus-internal state.

## Known limitations

- **Gemini CLI has never gotten past `session/new`** on any machine
  tested so far — blocked by `IneligibleTierError: UNSUPPORTED_CLIENT`,
  a free/individual-tier Google account restriction, not a chorus bug.
  The entire `CreateTerminal` RPC path (`internal/acpclient`) is fully
  implemented and unit-tested but has ZERO live evidence behind it,
  since neither Claude nor opencode use it (both run shell commands
  inside their own process) — Gemini is the one agent expected to
  exercise it for real. See the verification checklist below for what
  to test the moment it's reachable.
- **`routing.mode: llm` is unverified against real agents**: whether
  any agent advertises a model-switch command via
  `available_commands_update` at all is unconfirmed (the common case
  may be "agent only, no model switch"); the invocation syntax for a
  discovered model-switch command is a guess
  (`AvailableCommandInput.Unstructured.Hint` is free text, not a
  schema); whether a hidden routing-decision turn triggers
  prompt-injection suspicion in some agent (observed once for the
  delegation briefing, mitigated by reusing the same self-identifying
  framing, not guaranteed to transfer) is untested; whether
  `SessionUsageUpdate` (auto-compaction's trigger) is emitted reliably
  by Gemini/opencode, not just Claude, is unconfirmed.
- **ACP session modes (`modes`/`mode`/`auto`) are unverified against real
  agents** — no session in this project's testing has exercised
  `current_mode_update` live, so whether Claude/Gemini/opencode
  advertise session modes at all, and under what id/name (e.g. whether
  Claude Code's "accept edits"/"bypass permissions" concept is exposed
  this way over ACP), is unconfirmed. `agents.yaml`'s `auto_mode` is
  deliberately never guessed or defaulted — discover the real value via
  `modes` before setting it.
- **opencode runs with zero configured credentials by default**,
  silently falling back to its own free hosted backend
  (`providerID=opencode`, `model=big-pickle`) — not the
  subscription/login model the rest of chorus assumes. `opencode
  providers login` first if you want it backed by a real provider.
- **The delegate token is visible to each agent's own subprocess**
  (necessarily, to reach the Hub) — if an agent's own CLI logs its RPC
  parameters verbosely (confirmed: Gemini does, for at least its error
  paths), the token could leak into that agent's own logs. Switching
  the Hub to named pipes/Unix sockets was considered and rejected as
  disproportionate scope; documented residual risk.
- **Delegation reply collection has a theoretical, never-observed
  race**: if the very last streamed chunk of a sub-session's reply is
  still sitting in `outputCh`'s buffer when the RPC response arrives,
  `Collectors.Collect()` could return slightly truncated text.
- **Inbound image rendering is unit-tested only** — no agent so far has
  actually sent image content back (outbound IS confirmed live).
  `Connection.SupportsImagePrompts` is recorded but nothing reads it —
  no warning if you attach an image to an agent that never advertised
  support for receiving one.
- **Ctrl+V clipboard-image paste's macOS/Linux code paths are unverified
  live** — this codebase is developed entirely from a Windows
  environment, so `clipboardimage_darwin.go` (`osascript`/`pbpaste
  -Prefer png`) and `clipboardimage_linux.go` (`wl-paste`/`xclip`) are
  written from documented CLI behavior only, unit-tested only insofar as
  `readClipboardImage` is a stubbable function var (same pattern as
  `writeClipboard`). The Windows path (direct CF_DIB decode via
  user32/kernel32 syscalls, `dibToPNG`) IS confirmed, including a
  pixel-level round-trip test of the bottom-up/BGR decode. Run the first
  real macOS/Linux session through this feature and update this entry
  either way — including "confirmed working," the same convention this
  section uses everywhere else.
- **The bubbletea TUI has been built and is covered by unit tests, but
  the tool-invocation environment used to build it has no real PTY** —
  most of bubbletea's actual terminal rendering (alt-screen, keyboard/
  mouse behavior, spinner animation, glamour styling) is confirmed only
  by specific direct user reports (Esc-interrupt, click-drag selection,
  arrow-key menus, the textarea input box), not a full pass. Run it
  interactively before assuming a UI change looks right.
- **No checkpointing/rewind** — deliberately out of scope so far:
  durable per-turn snapshots, a restore UI, and an unanswered design
  question (what does "restore" mean across N independently-running
  agent sessions?) make this a bigger, separate feature.
- **Sandboxing (`sandbox/`) is confirmed live** for Claude, Gemini, and
  opencode (both free-tier and a VPN-bound Ollama backend) — see
  `sandbox/README.md` and the per-agent READMEs for setup. Cross-agent
  delegation is NOT supported for a sandboxed agent.
- **Headroom compression proxy (`internal/headroom`, 2026-09): confirmed
  live for Claude, on Rancher Desktop/Windows (WSL2 backend)** — two
  go:build-tag-gated live tests (see "Test coverage" above) exercised
  the real thing: `internal/headroom.Start`/`Stop` against the real
  published image, and a real `claude-agent-acp` subprocess redirected
  via `ANTHROPIC_BASE_URL` at a real Headroom container, prompted for
  real. Confirmed by this: (1) `claude-agent-acp` DOES honor
  `ANTHROPIC_BASE_URL` — Headroom's own `/stats` showed real compressed
  Anthropic traffic (`"agent":"claude-code"`, real token counts, a real
  provider-side prompt-cache hit on a second run) after the redirect;
  (2) Headroom's proxy does NOT need its own copy of a provider API key
  for this — neither `ANTHROPIC_API_KEY` nor `OPENAI_API_KEY` was set
  anywhere in the test environment, and it still worked, confirming
  `providerKeyEnvVars`'s doc comment's "not required" claim rather than
  just asserting it; (3) `host.docker.internal` from a separate
  container DOES reach the published port on this backend. This testing
  is ALSO what found and fixed the docker-run command bug and the
  `slim`-variant crash documented in internal/headroom's own entry
  above — both invisible to the stubbed-runDocker unit tests, only
  caught by these live ones.
  Still unconfirmed: the same `host.docker.internal` path specifically
  on Rancher Desktop's native-Linux backend (a materially different
  network path — containers reach the host over a real bridge interface
  there, not Windows/macOS's VM-proxied networking, which is why the
  container's port is published broadly rather than loopback-only in
  the first place — see Start's own doc comment); whether opencode's own
  multi-provider config accepts a custom base URL the same way; and
  Gemini entirely, moot regardless since it's already blocked by the
  free-tier `IneligibleTierError` above. Update this entry with whatever
  the Linux/opencode runs find, the same convention every other item in
  this list uses.
- **`session.Connection.Close()` may not kill an `npx`-spawned agent's
  actual Node.js process on Windows** — found live (2026-09) as a side
  effect of the Headroom live testing above: after `Close()`, the
  spawned subprocess's own cwd stayed locked (`os.RemoveAll` failing
  with "used by another process") for several seconds, consistent with
  `cmd.Process.Kill()` only reaching the immediate `npx` wrapper, not
  the Node.js process `npx` forks underneath it. Not Headroom-specific —
  affects any `npx`-based `spawn` entry (the default `claude` entry
  included) on Windows; could mean `quit`/Ctrl+C leaves an orphaned Node
  process running rather than a merely-annoying leaked file lock. Not
  yet root-caused beyond confirming the symptom — likely fix is tracking
  and killing the real process tree (Windows job objects, or
  `taskkill /T`) rather than a bare `Process.Kill()`.

## Gemini verification checklist — run the moment `gemini --acp` works

Everything in this repo has been built and verified against Claude and
opencode only. The core ACP plumbing (spawn, prompt, render,
permissions, routing) is protocol-generic and should transfer by
construction, but several features are gated on ACP capabilities each
agent opts into individually. Check in order, and update "Known
limitations" above with whatever you find — including "confirmed
working exactly like the others," which is itself worth recording:

0. Sanity-check `gemini --acp` directly, outside chorus, first.
1. `go build`/`go vet`/`go test` clean (confirms the baseline only —
   doesn't verify anything Gemini-specific).
2. `./chorus` → `gemini ready (session ...)`, no `IneligibleTierError`.
3. `capabilities` → record `loadSession`/`promptCapabilities.image`
   (Claude and opencode both report `true` for each).
4. `gemini: say hi in one word` → streams correctly.
5. A small edit → tool call renders, permission prompt appears with
   Gemini's real options, diff renders correctly on approval.
6. **The important one**: ask Gemini to run a real multi-step shell
   command. Watch whether `CreateTerminal` gets invoked at all (add a
   temporary log line if you need to confirm — remove before
   committing) and whether the permission gate fires for it. This is
   the first live confirmation either way, since no agent tested so
   far calls this RPC at all.
7. `quit`, restart with `--resume` → `gemini resumed`, and it actually
   remembers something told to it earlier (if `loadSession` was `true`
   in step 3; `ready` is correct, not a failure, if it was `false`).
8. After Gemini's had one turn, `commands` → test the ambiguous-owner
   `/name` flow if it shares a command name with Claude/opencode.
9. `gemini: what color is this? @image.png` → confirms real image
   bytes are processed, not just the file path as text.
10. Delegation both directions, if `delegation.enabled: true`.
11. An unprefixed prompt under `routing.mode: llm` → confirms it routes
    to Gemini when appropriate.
12. `modes` → does Gemini report any ACP session modes at all? If so,
    record the real id/name here and in "Known limitations" above —
    this whole mechanism has zero live confirmation from any agent so
    far.

## Cross-agent delegation & cost model

Delegation is **opt-in, off by default** (`agents.yaml`'s
`delegation.enabled`) — live use found agents rarely call `delegate` on
their own even with nudging in place, and the `delegate` MCP tool's
schema costs ~500-1400+ tokens on every turn of every agent it's
attached to, whether used or not. The project's primary cost lever is
LLM-based agent+model routing (`routing.mode: llm`) plus auto-compaction,
not delegation.

When enabled: `agents.yaml`'s `delegation.prefer` (ACP ToolKinds) and
`nudge_threshold` control when a `metered` agent gets a one-time,
chorus-authored nudge (queued as its next turn, same mechanism as the
seeded briefing) suggesting it delegate to an idle non-metered agent
instead of continuing direct mechanical work. `stats` (REPL command)
shows real per-agent direct-vs-delegated tool call counts and each
agent's most recently reported token usage, to tune this against real
numbers instead of guesswork.

`!<command>` runs a shell command directly, bypassing every agent
entirely — for deterministic work (`go build`, `go test`, etc.) that
doesn't need an LLM turn at all, not even a free one.

**Non-goal**: not "maximize delegation volume" — a delegate reply is
still an unverified draft, so judgment-heavy work that depends on
context only the metered agent already holds doesn't get cheaper to
verify just because it was delegated.
