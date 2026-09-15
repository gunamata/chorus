# chorus

> **⚠️ Experimental, not production-ready.** chorus is a personal/
> exploratory project. Interfaces, defaults,
> and behavior can change without notice between versions, several
> features are unit-tested but not yet confirmed working in a real
> terminal (see [Known limitations](#known-limitations)), and it hasn't
> had the kind of broad real-world usage that would surface issues a
> single developer's testing wouldn't catch. Use at your own risk —
> don't point it at anything you can't afford to have go sideways, and
> review what `agents.yaml` lets each agent do (see
> [Permission policy](#permission-policy-agentsyaml)) before trusting it
> with real work.
>
> Feedback, bug reports, and contributions are very welcome — this is
> exactly the stage where they're most useful.

A single foreground CLI that owns several ACP agent sessions — Claude
Code, Gemini CLI, and opencode. Type commands, they route to whichever
agent you name, output streams back live, and permission questions
surface as a normal prompt in the same terminal. Quit and come back
later with `--resume` — see [Session persistence](#session-persistence)
— and supported agents pick up where you left off instead of starting
cold.

No persistent daemon, no API keys required by chorus itself — Claude
and Gemini authenticate via whatever you're already logged into
(`claude` CLI subscription, `gemini` CLI Google login). opencode is a
partial exception: see [Prerequisites](#prerequisites). One narrow,
documented exception to "no sockets" exists to support cross-agent
[delegation](#cross-agent-delegation) — see that section.

## Status

v1 (core plumbing), v2 (auto-routing + registry), v3 (cross-agent
delegation), session persistence, slash-command passthrough, and image
attachments (both directions) are all built and verified live against
real Claude and real opencode. In particular, **Gemini's ACP
mode currently rejects free/individual-tier Google accounts** (a
Google-side restriction, not a chorus bug). opencode was added as a
third agent specifically so this repo stays usable on machines where
Gemini is blocked; keep both configured so you can try Gemini on a
machine where it works.

## Prerequisites

- Go 1.22+
- `claude` CLI, logged into an active subscription (`claude login`)
- `gemini` CLI, logged into a Google account with ACP access — `npm
  install -g @google/gemini-cli`, then run `gemini` once to log in.
  Note: as of this writing, free/individual-tier Google accounts are
  rejected by `gemini --acp` itself — this is a Google-side restriction,
  not a chorus bug or a login problem.
- `opencode` (`npm install -g opencode-ai`). Works out of the box with
  **zero login** — it silently falls back to its own free hosted
  default model, unrelated to any Claude/Anthropic subscription. If
  you want it backed by a specific provider instead, run `opencode
  providers login` first (interactive).
- Node/npm on `PATH` (used to run the Claude ACP adapter via `npx`,
  and as the runtime for `gemini`/`opencode`)

No `ANTHROPIC_API_KEY` or `GEMINI_API_KEY` required for the default
subscription/login path.

**Windows note:** if a locally-built chorus.exe (or even `go run`)
suddenly refuses to execute with no compiler error, check whether
Windows Smart App Control is blocking it (Settings > Windows Security
> App & browser control) before assuming it's a code bug — it can
silently reject unsigned local binaries.

## Install

Prebuilt binaries are published to
[GitHub Releases](https://github.com/gunamata/chorus/releases) for
macOS, Linux, and Windows (amd64 + arm64) on every tagged release — see
[Releases & publishing](#releases--publishing) for how those get built.

macOS / Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/gunamata/chorus/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/gunamata/chorus/main/install.ps1 | iex
```

Both scripts detect your OS/arch, download the matching release archive,
verify its checksum, and install into `~/.chorus/bin` (override with
`CHORUS_INSTALL_DIR`). On Windows, that directory is added to your user
PATH automatically (restart your terminal afterward); on macOS/Linux the
script prints the line to add to your shell profile if `~/.chorus/bin`
isn't already on PATH, rather than editing your dotfiles for you. Pass
`CHORUS_VERSION=vX.Y.Z` (env var on macOS/Linux, `$env:CHORUS_VERSION` on
Windows) to install a specific version instead of the latest release.
Run `chorus --version` afterward to confirm.

Both scripts also seed `~/.chorus/agents.yaml` from the release's
bundled default config — but **only on first install**: if that file
already exists, it's left completely untouched. This is what chorus
itself falls back to (see [Agent registry](#agent-registry-agentsyaml))
when no local `./agents.yaml` is present, so your own edits (models,
cost tiers, delegation/routing settings) persist across upgrades instead
of reverting to whatever a new chorus version happens to embed. Override
the location with `CHORUS_HOME` (must match whatever chorus itself uses
— see [Session persistence](#session-persistence)) if you want it
somewhere other than `~/.chorus`.

Seed a **custom** config instead of the bundled default with
`CHORUS_AGENTS=<path-or-url>` — same local-file-or-`https://`-URL
support as chorus's own `--agents` flag (see [Agent
registry](#agent-registry-agentsyaml)), and the same rule: `http://` is
refused, not just discouraged, since the seeded file becomes what chorus
execs `spawn` commands from unconditionally. Still only takes effect on
first install — an existing `~/.chorus/agents.yaml` is never overwritten,
`CHORUS_AGENTS` included; a failed/missing/non-https source falls back
to the bundled default with a warning rather than leaving the install
half-finished:

```sh
curl -fsSL https://raw.githubusercontent.com/gunamata/chorus/main/install.sh \
  | CHORUS_AGENTS=https://gist.githubusercontent.com/you/id/raw/agents.yaml sh
```

(the env var must be attached to `sh`, the process that actually runs the
downloaded script — attaching it to `curl` instead sets it for the wrong
command and it never reaches the install script at all)

```powershell
$env:CHORUS_AGENTS = "https://gist.githubusercontent.com/you/id/raw/agents.yaml"
irm https://raw.githubusercontent.com/gunamata/chorus/main/install.ps1 | iex
```

**Uninstall** with the matching `uninstall.sh`/`uninstall.ps1` (same repo
path, same `CHORUS_INSTALL_DIR`/`CHORUS_HOME` overrides). By default only
the binary (and, on Windows, its PATH entry) is removed — your
`agents.yaml`, session history, and any `chorus-headroom` Docker
container/volume ([Headroom compression
proxy](#headroom-compression-proxy-agentsyaml)) are left alone. Add
`CHORUS_UNINSTALL_PURGE=1` (`$env:CHORUS_UNINSTALL_PURGE = "1"` on
Windows) for a full wipe of all of it:

```sh
curl -fsSL https://raw.githubusercontent.com/gunamata/chorus/main/uninstall.sh | sh
```

```powershell
irm https://raw.githubusercontent.com/gunamata/chorus/main/uninstall.ps1 | iex
```

## Build

```sh
go build .   # produces ./chorus on macOS/Linux, chorus.exe on Windows
```

## Run

macOS / Linux:

```sh
./chorus                                        # every agent starts a fresh session
./chorus --resume                               # resume prior sessions where possible instead
./chorus --agents=agents.yaml.sandbox.linux     # use a different agents.yaml than the default
```

Windows:

```powershell
.\chorus.exe
.\chorus.exe --resume
.\chorus.exe --agents=agents.yaml.sandbox.windows
```

The rest of this doc uses the macOS/Linux `./chorus` form for brevity —
on Windows, swap in `.\chorus.exe`.

On startup chorus spawns all three agent subprocesses and creates (or,
with `--resume` — see [Session persistence](#session-persistence) —
resumes) an ACP session with each. If one fails to start (e.g. Gemini's
tier restriction), chorus prints a warning and continues with whichever
agent(s) started successfully.

### Usage

chorus runs as a real terminal UI (via
[bubbletea](https://github.com/charmbracelet/bubbletea)): a scrolling
output pane above a fixed input box at the bottom, on the terminal's
alt-screen — not a plain scroll-by REPL. The interaction itself is
unchanged, just the display:

```
[claude] fix the login bug in auth.py
[claude] Reading auth.py...
[claude] ● Read auth.py (completed)

PERMISSION: claude wants to: Edit auth.py
  1) Deny (reject_once)
  2) Allow Once (allow_once)
  3) Always Allow (allow_always)
[claude] Fixed the null check on line 42.
──────────────────────────────────────────────
❯ _
```

That first line — `[claude] fix the login bug in auth.py` — is chorus echoing your own submitted prompt back into the scrollback, tagged by which agent it went to, on a solid highlighted background (not visible in this plain-text example, but real in the actual TUI). ACP has no way to do this for you on a live turn (ask returns the reply, not an echo of what was asked), so chorus does it explicitly — without it, a long session would give you replies with no record of which question each one answers.

- Type a prompt with no prefix and chorus **routes** it (see
  [Routing](#routing-agentsyaml) below — off by default, straight to
  `default_agent`; optionally an LLM-based decision). `<agent>: <text>`
  always overrides routing and sends straight to that agent's persistent
  session (`claude`, `gemini`, or `opencode`).
- `!<command>` runs a shell command directly — no agent involved at all
  (see [Native commands](#native-commands-)).
- All started agents run concurrently — output from any of them can
  appear at any time, including mid-turn, interleaved in the same
  scrolling pane.
- **Scroll the output pane** with `PgUp`/`PgDn`, `ctrl+u`/`ctrl+d`
  (half-page steps), or the mouse wheel — it auto-follows new output
  while you're at the bottom, and stays put (doesn't get yanked back
  down) if you've scrolled up to read something while agents keep
  streaming (except a permission/routeAsk menu, which always forces the
  view back down to itself — it needs an answer before anything else can
  proceed, so it isn't allowed to scroll out of sight).
- **Click-drag to select and copy text.** Mouse wheel support means
  chorus, not your terminal, owns plain click-drag — so instead of
  leaving that dead, chorus implements copy-on-select itself: dragging
  over one or more lines highlights them live as you drag (whole lines, not
  partial — a deliberate line-level tradeoff) and releasing copies them
  straight to your system clipboard, with a brief "N lines copied"
  confirmation (the highlight stays visible until your next keypress).
  If you'd
  rather have your terminal's own native selection instead (e.g. for
  exact character/column ranges), set `CHORUS_DISABLE_MOUSE=1` before
  starting chorus — this gives up mouse-wheel scrolling (PgUp/PgDn/
  ctrl+u/ctrl+d keep working) in exchange for the terminal handling
  click-drag itself again. Without that env var, your terminal's own
  override modifier (commonly **Shift+drag**, e.g. on Windows Terminal)
  may still reach past chorus's mouse capture too, depending on the
  terminal.
- **The input box is multi-line** and wraps long text instead of
  scrolling sideways forever. Press **ctrl+j**, or end the line with a
  trailing **`\`** before Enter, for a new line within the same prompt
  (Enter always submits otherwise) — two terminal-agnostic ways in, since
  Shift+Enter isn't reliably decoded as a distinct key on every terminal.
  `Home`/`End` (and, for the same job, `ctrl+a`/`ctrl+e` — macOS Terminal/
  iTerm's own readline-style bindings) jump to the start/end of the
  current line. `↑`/`↓` move the cursor between lines like any multi-line
  editor — and once the cursor is already on the topmost or bottommost
  line, they instead walk through **prompt history** (everything you've
  submitted this session, most recent first), the same way a shell's `↑`
  does.
- **Pasting a large block of text** (over ~800 characters, or more than 3
  lines) collapses to a short
  `[Pasted text #N +NN lines]` placeholder in the input box instead of
  ballooning it to dozens of wrapped lines. The full text is still sent
  exactly as pasted once you submit — only the input box and scrollback
  echo show the short form.
- **Typing `/` at the very start of the input** opens a live, filterable
  popup of every agent-advertised slash command matching what you've
  typed so far, with descriptions — narrows as you keep typing,
  navigate with **↑/↓**, accept with **Tab or Enter**. Typing **`@`**
  anywhere opens the same kind of popup for files in the current
  directory (directory-scoped, not a whole-repo fuzzy search — selecting
  a directory keeps the popup open one level deeper, matching shell
  Tab-completion). **Esc** dismisses either popup without interrupting a
  running turn.
- **`ctrl+v` attaches an image straight from your OS clipboard** — copy a
  screenshot, then paste it directly into chorus, no need to save it to
  a file first (see
  [Images](#images)). If the clipboard doesn't currently hold an image,
  `ctrl+v` is a silent no-op (your terminal's own text-paste handling,
  via bracketed paste, is unaffected either way).
- Once a prompt finishes, chorus prints `[agent] finished in <duration>`
  (`45s`, `2m14s`, `1h05m`) — and while one or more agents are still
  working, a spinner + elapsed-time line for each of them stays visible
  just above the input box the whole time, so a long-running turn with
  no output yet doesn't look identical to nothing happening.
- Permission prompts render whatever option set the agent actually
  sent (never a fixed menu). Answer with **arrow keys (↑/↓) + Enter**,
  or by typing the option's number, its name/kind (case-insensitive), or
  `cancel` — both work interchangeably, even in the same prompt. The
  routeAsk agent-choice menu (ambiguous auto-routing, or an ambiguous
  slash command) works the same way.
- **`Esc` interrupts the current turn** without ending the session —
  sends ACP's own `session/cancel` for whichever agent you're most
  likely watching (the one most recently dispatched, if it's still
  busy; otherwise every currently-busy agent). The busy-status line
  above the input box shows "(esc to interrupt)" whenever it's actually
  actionable.
- **`ctrl+c` interrupts EVERY currently-busy agent**, not just the one
  Esc would narrow to — a broader "stop everything running right now,"
  and the input box is left exactly as typed either way (nothing is
  cleared while something's still busy). Only once nothing is running
  does `ctrl+c` fall back to acting on the input box: the first press
  with a non-empty draft clears it, and only once the input is already
  empty does a `ctrl+c` press quit — cleanly ending every session and
  restoring your terminal (leaves the alt-screen, cursor visible), same
  as typing `quit`/`exit`. That two-step (clear, then quit) means an
  instinctive "clear what I typed" keystroke can't accidentally kill
  every connected agent's session in one press.
- **A turn finishing while the terminal is unfocused rings the terminal
  bell** (`\a`). Silent while focused; nothing to configure (a
  terminal that doesn't report focus at all just never triggers it,
  rather than risking a bell on every single turn).
- Full reasoning/thinking text is **hidden by default** — while an agent
  is thinking you see a brief `[agent] ⠋ Pondering…`-style indicator (a
  random word from a small set, spinner-animated) instead
  of a wall of chain-of-thought. Type `thoughts` to toggle full
  `(thinking)`-prefixed reasoning text on or off.
- `commands` lists every agent's currently known slash commands (see
  [Slash commands](#slash-commands)).
- `@path/to/image.png` anywhere in a prompt attaches that image (see
  [Images](#images)).
- `capabilities` shows what each connected agent actually advertised at
  initialize (session resume support, image-prompt support) — useful
  when verifying a newly-added or previously-untested agent.
- `stats` shows each agent's direct tool calls vs. `delegate` calls sent
  and received so far this run — real numbers to tune
  `delegation.prefer`/`nudge_threshold` against (see
  [Cross-agent delegation](#cross-agent-delegation) below) instead of
  guesswork. Also shows each agent's most recently reported token usage
  (`used/context-size (N%)`, from ACP's own `usage_update` — the same
  data behind the dim `(tokens: ...)` line you see stream past inline,
  captured here so you don't have to scroll back to find the last one).
  An agent that's had a real conversation but never called a tool still
  shows up for its token usage alone.
- `modes` lists each connected agent's [ACP session
  modes](https://agentclientprotocol.com/protocol/session-modes) (e.g.
  Claude Code's "accept edits"/"bypass permissions" concept, if the
  agent advertises it this way) and which one is currently active. `mode
  <agent> <id-or-name>` switches explicitly; `auto`/`auto <agent>`
  toggles into (and back out of) whichever mode `agents.yaml`'s
  `auto_mode` names for that agent. See [Auto mode](#auto-mode-acp-session-modes)
  below — mode IDs/names are entirely agent-defined, so chorus never
  guesses one for you.

Agent replies and thoughts are rendered as styled markdown (bold,
headers, code blocks — via [glamour](https://github.com/charmbracelet/glamour)),
redrawn in place as each chunk streams in. In-progress tool calls show
an animated spinner.

## Slash commands

Each agent's own slash commands still work — chorus doesn't hide them,
it makes them discoverable across all your agents at once:

```
> commands
claude:
  /compact              Summarize and compact the conversation history
  /init                 Initialize a new CLAUDE.md file with codebase documentation
  ...
opencode:
  /init                 guided AGENTS.md setup
  /review               review changes [commit|branch|pr], defaults to uncommitted

> /review
(/review -> opencode)
[opencode] ● review changes [commit|branch|pr], defaults to uncommitted (completed)
...

> /init

more than one agent has a "/init" command — which did you mean?
  1) claude
  2) opencode
> 1
[claude] ● Initialize CLAUDE.md (in_progress)
...
```

A bare `/name ...` (no agent prefix) is routed straight to whichever
agent(s) declared that command via their own `available_commands_update`
— you never had to know or type which agent owns it. If exactly one
agent has it, it goes straight there; if more than one does, chorus
asks which you meant, the same way it does for ambiguous routing.
`<agent>: /name ...` still works too, and always wins.

You don't have to already know a command's exact name either: typing
just `/` opens a live popup listing every matching command (narrowing as
you keep typing) with its description and, when more than one agent
declares it, which agents — `↑`/`↓` to move, `Tab` or `Enter` to accept.

One timing nuance: an agent's commands are only known once it's
reported them at least once — this has been observed to sometimes
happen only after that agent's first prompt, not automatically at
startup. A command typed before an agent has ever spoken won't be
attributed to it yet and just falls through to normal routing instead
of erroring.

## Images

Attach a local image to any prompt by referencing it with `@`:

```
> claude: what's wrong with this layout? @screenshot.png
[claude] The button is overflowing its container because...
```

The file is read, base64-encoded, and sent as real image content
alongside your text — not a text description of the path. Supported
extensions: `.png`, `.jpg`/`.jpeg`, `.gif`, `.webp`. A token that
doesn't resolve to a readable file is left in the prompt text
untouched (with a warning printed) rather than silently dropped.
Typing `@` opens a live popup listing matching files/directories in the
current directory as you type (directory-scoped, not a full-repo fuzzy
search — selecting a directory keeps the popup open one level deeper,
the same way shell Tab-completion works), so you don't need to already
know the exact path.

`@` also works for non-image files: a token that resolves to a real,
reasonably-sized (under 200KB) file that ISN'T an image gets its content
spliced directly into the prompt text as a fenced block, rather than
being sent as image content. An `@`-token that doesn't resolve to
anything real (an email address, a stray `@mention`) is silently left as
plain text — no warning, since that's the common, expected case for a
generic pattern this broad.

**Pasting an image straight from your OS clipboard** works too — press
`ctrl+v` after copying a screenshot (or any image) and chorus saves it
under this project's `images/` directory, then attaches it exactly like
a real `@path` reference. No need to save the screenshot to a file
yourself first. Confirmed working on Windows; the macOS (`pbpaste`) and
Linux (`wl-paste`/`xclip`) code paths are implemented but not yet
verified live, since this codebase is currently developed from a Windows
environment — if the clipboard simply doesn't have an image, `ctrl+v` is
a silent no-op either way.

If an agent sends an image back, it's saved under this project's
`images/` directory (see [Session persistence](#session-persistence) for
where that lives) and the path is printed — a terminal can't display the
bytes inline,
but a path you can open beats a bare `[image]` placeholder.

## Native commands (`!`)

```
❯ !go test ./...
[!] go test ./...
ok  	chorus	(cached)
ok  	chorus/internal/render	0.23s
...
[!] done in 1.8s
```

`!<command>` runs a shell command directly on this machine — `cmd /C`
on Windows, `sh -c` elsewhere — completely bypassing every connected
agent, metered or free. It's for exactly the class of work chorus's
agents themselves keep running as plain tool calls: `go build`,
`go vet`, `go test`, `gofmt -l`, a lint pass, checking whether a file
exists. That work is deterministic and needs no reasoning — spending
an LLM turn on it (even a free-tier one) is worse than free, since it
adds latency and a chance the "summary" of the result doesn't match
what actually happened. `!` gives you (or an agent you ask to use it,
via its own shell tool) a way to get the exact same real output a
terminal would show, without going through anyone's model.

A couple of things worth knowing:

- It never asks for permission — you typed the exact command
  yourself, so there's nothing for chorus to gate the way it gates an
  *agent's* tool calls (`agents.yaml`'s `execute` kind). This is the
  same trust boundary as running the command in a plain terminal next
  to chorus; `!` doesn't grant any access chorus didn't already have.
- Output is captured (stdout+stderr combined), capped at 200KB, and
  the command can't read from stdin (it's disconnected — bubbletea
  owns the real terminal input while chorus is running), so anything
  that waits on input will just run until a 5-minute timeout instead
  of hanging forever.
- It runs asynchronously, same as an agent prompt — you can keep
  interacting with agents (or run another `!` command) while a long
  one is still going.

## Auto mode (ACP session modes)

ACP has a real protocol mechanism for this —
[session modes](https://agentclientprotocol.com/protocol/session-modes):
an agent can advertise a list of named modes (id + human-readable name,
e.g. Claude Code's own "accept edits"/"bypass permissions" concept) and
lets a client switch between them mid-session via `session/set_mode`.
chorus surfaces this directly rather than guessing at any agent's
specific mode name:

```
> modes
claude:
    default (default)
  * acceptEdits (Accept Edits) — auto-accept file edits without asking

> mode claude acceptEdits
claude: mode set to acceptEdits

> auto claude
claude: auto mode off        # toggled back, since it was already in that mode
```

- **`modes`** lists every connected agent's advertised modes and marks
  the currently active one with `*`. An agent that hasn't reported any
  (no `AvailableModes` in its `session/new`/`session/load` response)
  simply doesn't appear — this is not an error, it just means that
  agent has nothing here to offer, or hasn't reported it yet (same
  discovery-timing caveat as [Slash commands](#slash-commands)).
- **`mode <agent> <id-or-name>`** switches explicitly — matches either
  the short id (`acceptEdits`) or the human-readable name (`Accept
  Edits`, case-insensitive), so you don't have to remember which form a
  given agent uses.
- **`auto` / `auto <agent>`** toggles a configured agent into (and back
  out of) its own "auto"/"accept edits"/"yolo"-equivalent mode, driven
  by an `auto_mode` value you set yourself in `agents.yaml` (see below)
  — chorus remembers whatever mode was active before switching, so a
  second `auto`/`auto <agent>` restores it rather than needing you to
  look it up again. With no argument, `auto` applies to every connected
  agent that has `auto_mode` configured.

**chorus never hardcodes or guesses a mode string for any agent** — mode
ids/names are entirely agent-defined, and (as of this writing) unconfirmed
for Claude Code, Gemini CLI, and opencode specifically, since no session
in this project's own testing has exercised `current_mode_update` live.
Use `modes` to discover the real value for your agent and setup, then set
it once in `agents.yaml`:

```yaml
agents:
  - name: claude
    spawn: ["npx", "-y", "@agentclientprotocol/claude-agent-acp"]
    auto_mode: acceptEdits   # optional — the id/name `modes` showed you
```

Leaving `auto_mode` unset (the default) just means `auto`/`auto <agent>`
has nothing configured to switch that agent to — `mode <agent> <id>`
still works either way, since it doesn't depend on this field at all.

## Agent registry (`agents.yaml`)

**`agents.yaml` is chorus's only config file** (2026-08-25 — `policy.yaml`
has been eliminated; everything that used to live there — the permission
allow-list, routing, delegation — now lives in this one file). Which
agents chorus spawns, their permissions, and how prompts get routed to
them all live in data, not code:

```yaml
default_agent: opencode

delegation:
  enabled: false      # off by default — see Cross-agent delegation below
  briefing: true
  prefer: [execute, edit]
  nudge_threshold: 2

compaction:
  enabled: false      # see Auto-compaction below
  threshold_percent: 60
  aliases: [compact, summarize, condense]

routing:
  mode: off           # off | llm — see Routing below
  decision_agent: opencode
  context_level: digest
  anonymize: true     # redact emails/IPs/tokens before the decision agent sees them — see Routing below

headroom:
  enabled: false      # off by default — see Headroom compression proxy below
  port: 8787

agents:
  - name: claude
    spawn: ["npx", "-y", "@agentclientprotocol/claude-agent-acp"]
    cost_tier: metered
    notes: "general-purpose, metered usage"
    transport: acp
    auto_allow: [read, search, think]
    auto_allow_tools: [mcp__chorus-delegate__]
    models:
      - id: claude-opus-4-8
        label: Opus
        capabilities: "highest reasoning quality, slowest, most expensive"
        when_to_use: "complex/ambiguous architecture, high-stakes correctness"
      - id: claude-sonnet-5
        label: Sonnet
        when_to_use: "the default for most implementation/debugging work"
  - name: gemini
    spawn: ["gemini", "--acp"]
    cost_tier: seat
    notes: "seat/subscription usage"
    transport: acp
    auto_allow: [read, search, think]
    auto_allow_tools: [mcp__chorus-delegate__]
  - name: opencode
    spawn: ["opencode", "acp"]
    cost_tier: free
    notes: "free by default, zero configured credentials"
    transport: acp
    auto_allow: [read, search, think]
    auto_allow_tools: [mcp__chorus-delegate__]
```

Add an agent by adding an entry — nothing else needs to change.
`transport` must be `acp` (the only one chorus implements). Entries
are tried in file order at startup; one failing (e.g. Gemini's tier
block) doesn't stop the others. `cost_tier` and `notes` are free text
you edit to describe your own setup — they're surfaced to every OTHER
connected agent via the `delegate` tool's description, the seeded
first-turn briefing, and (if enabled) the LLM routing decision, so an
agent has concrete signal for deciding whether/to whom to delegate, or
which agent+model fits a given prompt. `models` is optional per agent —
omit it entirely (as the registry's own `gemini`/`opencode` entries do)
to let LLM-based routing pick that agent but never attempt to switch its
model. `auto_mode` is also optional per agent — see [Auto
mode](#auto-mode-acp-session-modes) above. `env` is an optional per-agent
map of extra environment variables for a bare (non-sandboxed) spawn —
see [Headroom compression proxy](#headroom-compression-proxy-agentsyaml)
for the motivating use case.

**`agents.yaml` isn't actually required on disk.** Resolution order,
with no `--agents` flag:

1. A local `./agents.yaml` in the directory you run chorus from — for a
   per-project config that differs from your usual setup.
2. A central `~/.chorus/agents.yaml` (as of 2026-09-03 — override the
   root with `CHORUS_HOME`) — the file `install.sh`/`install.ps1` seed
   once on first install (see [Install](#install)) and never touch
   again, so your own edits survive an upgrade.
3. The embedded default (this repo's own reference config, baked into
   the binary at build time) — the final fallback, so `chorus` still
   works out of the box with zero setup even without ever having run an
   install script (e.g. a plain `go build`).

Each step only runs if the previous one found nothing; the first match
wins.

**More than one config can coexist via `--agents=<path-or-url>`** — e.g.
keep a plain `agents.yaml` for normal use and a separate sandboxed one
(this repo ships `agents.yaml.sandbox.windows`/`.macos`/`.linux` — see
[Sandboxing](#sandboxing-containers)) for a containerized run, switching
per-invocation instead of renaming/swapping files:

```sh
./chorus                                      # local -> central -> embedded, first match wins
./chorus --agents=agents.yaml.sandbox.linux   # loads that local file explicitly instead
./chorus --agents=https://gist.githubusercontent.com/you/id/raw/agents.yaml # or fetch one remotely
```

`--agents` always wins over the three local-resolution steps above, and
unlike them, a missing/unreachable source is a hard error rather than a
silent fallback — you asked for that specific one.

**`--agents` also accepts an `https://` URL** (2026-09) — e.g. a raw
GitHub Gist link — fetched fresh over the network on every launch, never
cached to disk, so editing the remote source takes effect on your very
next run with no separate update step. Plain `http://` is rejected
outright, not just discouraged: see the next paragraph for why that
matters more here than it might elsewhere. Capped at 1 MiB and a 15s
timeout so an unreachable or misbehaving host doesn't hang startup or
balloon memory.

**`agents.yaml` is trusted, executable configuration, not passive
data** — `spawn` is a literal command line chorus runs unconditionally
at startup. Never point chorus at an `agents.yaml` (local or remote) you
didn't write or don't fully trust; using someone else's is equivalent to
running a script they handed you. Same trust model as a Makefile or a
VS Code `tasks.json` — a **remote URL is a strictly bigger version of
that same risk**, since unlike a local file, its content can change
between runs without you touching anything, and (over plain `http://`)
could be tampered with in transit by anyone on the network path. Only
point `--agents` at an `https://` URL you control or fully trust, the
same way you'd think twice before piping a random URL into `sh`.

## Permission policy (`agents.yaml`)

Each agent's `auto_allow`/`auto_allow_tools` (moved here from the old
`policy.yaml`) is the permission allow-list, keyed on ACP's standardized
`ToolCallUpdate.Kind` (`read`, `edit`, `delete`, `move`, `search`,
`execute`, `think`, `fetch`, `switch_mode`, `other`) since that's the one
thing every agent reports uniformly — not on per-agent tool names, which
aren't visible over ACP. `auto_allow_tools` matches by the tool call's
title (case-insensitive **prefix**, not substring — see
[Security](#security) for why) instead of kind — needed for chorus's own
`delegate` tool, since an external MCP tool's kind is generically `other`
to every agent, so kind-based matching can't single it out.

Anything not listed under either falls through and asks.

**This allow-list is enforced, not advisory**: `edit`/`execute` (and
everything else not explicitly listed) require your approval even for
agents that route file writes and shell commands through chorus's own
client-owned RPCs rather than doing it internally — see
[Security](#security).

## Routing (`agents.yaml`)

A prompt with no `<agent>: ` prefix is routed one of two ways, set by
`routing.mode`:

- **`off`** (the code's default when unset) — always goes straight to
  `default_agent`. No keyword matching, no LLM call.
- **`llm`** — `routing.decision_agent` (should be a non-metered agent;
  chorus warns, doesn't refuse, if it isn't) is asked, via a hidden
  sub-session, to pick both an agent AND, optionally, a model tier from
  each agent's declared `models`, given the prompt and some amount of
  recent activity (`routing.context_level`: `prompt` sends none, `digest`
  — the default — sends a short rolling summary, `full` sends everything
  chorus has retained). This is chorus's replacement for the earlier
  keyword-based router (deliberately removed entirely, not kept as a
  cheaper alternative mode) — a real judgment call by a cheap agent, not
  a fixed word list. Type `context` in the REPL to see/change the context
  level live, without restarting.

Either way, **an explicit `<agent>: text` prefix or a recognized slash
command always bypasses routing entirely** — you asked for that agent
specifically, chorus never second-guesses an explicit ask. If the LLM
router's decision names an unknown agent, times out, or its reply can't
be parsed (it has to ask the decision agent to reply in a specific JSON
shape over plain text, since ACP has no structured-output primitive —
this is inherently best-effort), chorus falls back to `default_agent`
rather than failing the prompt.

A failed decision (unknown agent, timeout, or a reply that couldn't be
parsed as JSON) prints `[routing] decision failed (...) — falling back
to <default_agent>` so a fallback is never silent.

`routing.decision_timeout_seconds` (default 60) bounds how long chorus
waits for the decision agent before giving up and falling back. Found
live: a free/shared-capacity decision agent's own upstream provider can
be intermittently slow or need an internal retry — the original 25s
default cut those off, surfacing as a generic `decision failed`
(`Internal error`). If you see that repeatedly, check the decision
agent's own log first — for opencode,
`~/.local/share/opencode/log/opencode.log` — before assuming it's
chorus; raise this setting if it turns out to be provider-side
slowness.

`routing.anonymize` (default `true`) redacts obvious sensitive
patterns — email addresses, IPv4 addresses, API-key/token-shaped
strings (`sk-…`, `ghp_…`, `AKIA…`, `Bearer …`, and any other 24+ char
run of letters/digits/underscore) — out of the prompt and
`context_level` activity before either reaches `decision_agent`, since
that agent is picked for routing cost/capability reasons, not
necessarily one you'd otherwise trust with the raw prompt. Only applies
to this hidden routing-decision call — never to what's actually sent to
the agent that ends up handling the real work. Pattern-matching, not
real PII detection: it can't tell a secret from a long git SHA or
identifier (redacts it anyway, harmless) and won't catch a short,
low-entropy credential the named prefixes don't cover. Set to `false`
if redaction is mangling legitimate prompt content and hurting routing
accuracy more than it's worth.

**Continuing a task on a different agent than the one that last handled
it** (whether via an LLM routing decision or you switching manually) gets
a one-time handoff: chorus prepends everything it's retained about the
task so far to the first prompt the new agent sees, self-identified as
automated context from chorus (not the user) — the same framing already
proven necessary to stop an agent from treating an unexplained
instruction message as a possible prompt injection.

## Auto-compaction (`agents.yaml`)

Long sessions accumulate context — left alone, some agents only compact
very late (and expensively). `compaction.enabled: true` makes chorus
proactively trigger a compaction-style command once an agent's reported
context usage crosses `threshold_percent` (default 60, based on research
into real-world agent usage patterns — compacting around 60% produces
much better, cheaper summaries than letting an agent wait until it's
nearly full). Never hardcoded as `/compact`: chorus only sends a command
an agent has actually advertised, matched against `compaction.aliases` by
substring — if nothing matches, it says so instead of guessing.
Deferred, not skipped, if the agent is mid-turn when the threshold is
crossed; fires once idle.

## Cross-agent delegation

**Off by default** (2026-08-25 — `delegation.enabled: false` unless set):
live use found agents rarely call `delegate` on their own, and attaching
the tool costs every agent ~500-1400+ tokens of schema on *every turn*,
whether it's ever used or not — paying that cost by default for a
feature that mostly sits idle wasn't a good trade. The mechanics below
are fully implemented either way (this project's own `agents.yaml` turns
it on, since it's chorus's own reference/testing setup for the feature)
— flip `delegation.enabled: true` to revisit it.

With delegation enabled and 2+ agents running, every agent's session
gets a `delegate(agent, task)` tool: one agent can hand a self-contained
sub-task to another and get back its text reply, instead of you manually
copy-pasting between them. Example: ask Claude to delegate a
summarization task to opencode, and Claude will call the tool itself if
it decides to.

- The delegated call runs in a **fresh, isolated sub-session** for the
  target agent — not its main conversation — so give it full context
  in the task text; the target has no memory of anything else.
- The sub-session's output (thinking, text, tool calls) does **not**
  appear in your interactive terminal — only a one-line log once it's
  done: `[delegate] claude -> opencode: "task text..." (2.3s, ok)`.
- **One hop only, by construction**: the sub-session gets no
  `delegate` tool of its own, so it cannot delegate further — this
  isn't a runtime check, the tool is simply never offered to it.
- Delegate calls are auto-allowed by default (see `auto_allow_tools`
  above) since they're not filesystem/shell operations themselves —
  whatever the target agent actually *does* with the task is still
  subject to the normal permission flow.

**Making delegation something agents actually reach for, not just
something that's possible:**

- The `delegate` tool's description lists every other connected
  agent's `cost_tier` and `notes` (from `agents.yaml`) so the calling
  model has concrete signal to weigh, not just bare agent names. It
  also explicitly frames delegation as more than analysis/summarize
  work: a well-specified implementation step (you've already decided
  *what* to change) is just as delegable as "explain this" once you
  hand over the exact file, change, and reasoning — the seeded
  briefing below carries the same framing.
- It also frames a delegate reply explicitly as **an unverified
  draft** — the calling agent stays responsible for checking it, which
  is what makes it safe to encourage proactive delegation in the first
  place.
- On a **brand-new** session (not a resumed one), once 2+ agents are
  connected, chorus seeds a one-time briefing message explaining the
  roster and encouraging the agent to delegate sub-tasks that fit
  another agent's profile rather than doing everything itself. This is
  a **real, visible turn** in your terminal (not hidden) — you'll see
  it and the agent's acknowledgment right after its "ready" line,
  before your first prompt. Because ACP session resume is real
  conversation history replay, this briefing persists automatically
  across every future resume of that session — it's sent once, ever,
  per session.
- Disable the briefing in `agents.yaml`:
  ```yaml
  delegation:
    briefing: false
  ```
  (On by default — delegation is meant to be something an agent
  reaches for on its own, not something you have to ask for every
  time.)
- If a session already existed before a 2nd agent became available
  (e.g. you ran chorus with just Claude, then later added opencode to
  `agents.yaml`), chorus tracks per-agent "has this session ever been
  briefed" independently of session resume/history, so a `--resume`d
  pre-existing session still picks up the briefing automatically on its
  next run. (This used to require starting over with no way to resume at
  all; fixed once it turned out to be a likely real cause of unreliable
  delegation, not just a theoretical gap.)
- **Delegation nudge**: ACP gives no way to redirect a tool call
  mid-permission-check (its response carries only allow/deny, no free
  text), so instead chorus counts how many direct tool calls a
  `metered` agent (`agents.yaml`'s `cost_tier`) makes in a row of a
  kind you've flagged as delegable, and once that streak crosses a
  threshold — while a non-metered agent is connected and currently
  idle — prepends a one-time suggestion to the metered agent's *next*
  turn naming the idle agent. The streak resets the moment the agent
  actually calls `delegate`. Off by default in the code (empty `prefer`
  list) — but **this project's own `agents.yaml` enables it**, since the
  briefing alone (a one-time message at session start) turned out not to
  be enough to make delegation reliable on a long session:
  ```yaml
  delegation:
    prefer: [execute, edit]   # ACP ToolKinds worth nudging about
    nudge_threshold: 2        # direct calls in a row before nudging (code default: 3)
  ```
  Type `stats` in the REPL to see real per-agent direct-vs-delegated
  counts and confirm this is actually shifting behavior, not just
  present in config.

This works by chorus re-invoking itself as a small local MCP server
(`chorus __mcp_delegate`) that each agent's own `mcpServers` config
spawns, which calls back into chorus's main process over a
127.0.0.1-only HTTP listener (random port and per-run token, never
touches the network). This is a deliberate, narrow exception to "no
sockets," unavoidable given how ACP's `mcpServers` mechanism actually
works: it spawns the delegate server from the *agent* subprocess, not
from chorus, so a callback path is the only way for it to reach back
into chorus's main process at all.

## Sandboxing (containers)

Opt-in filesystem/blast-radius containment and network-exfiltration
prevention: run an agent's subprocess inside a Docker container instead
of directly on the host, so its shell tool can only touch the mounted
project directory and only reach an explicitly allowlisted set of
hosts. Off by default — a normal `spawn` entry is unaffected.

A `workdir` field and two token substitutions in `spawn`'s argv make this
possible — chorus execs `spawn` directly with no shell involved, so
these are chorus's own substitution, not OS/shell environment expansion:

- `workdir` — the in-container path chorus tells the agent its cwd is
  (via ACP's own `Cwd` field), since a container has no way to resolve
  the host's real path.
- `{{CWD}}` anywhere in `spawn`'s argv is substituted with the real host
  project directory before chorus execs the command — lets a sandboxed
  `spawn` line (e.g. `docker run -v {{CWD}}:/workspace ...`) stay
  portable across clones/machines instead of hardcoding an absolute
  path.
- `{{ENV:NAME}}` anywhere in `spawn`'s argv is substituted with
  `os.Getenv("NAME")` — lets a `spawn` line reference a machine-specific
  host path (e.g. `{{ENV:HOME}}/.config/gcloud` for a mounted credential
  directory) without hardcoding a particular user's name into
  `agents.yaml`. An unset variable substitutes as an empty string.

```yaml
- name: opencode
  spawn: ["docker", "run", "--rm", "-i", "--cap-add=NET_ADMIN", "--cap-add=NET_RAW",
          "-v", "{{CWD}}:/workspace", "chorus-opencode-sandbox"]
  workdir: /workspace
  cost_tier: free
```

Ready-to-build sandbox images live under [`sandbox/`](sandbox/) — one
per agent, each following whichever mechanism is most official for that
agent rather than one uniform wrapper: Claude adapts Anthropic's own
official devcontainer directly; opencode is a custom image, since no
official one exists; Gemini was originally meant to use the CLI's own
built-in `GEMINI_SANDBOX=docker` sandboxing with no chorus-owned image
at all, but that's confirmed incompatible with `--acp` mode (its
relaunch logic deadlocks against `--acp`'s required persistent stdin
pipe — see `sandbox/README.md`), so `sandbox/gemini/` instead builds on
Google's own real sandbox image directly, the same adapted-official
pattern as Claude. See [`sandbox/README.md`](sandbox/README.md) for the
full picture, and each
subdirectory's own README for auth options (direct API key, AWS
Bedrock, Google Vertex AI, or a private endpoint) and the firewall's
`CHORUS_SANDBOX_ALLOW_HOSTS` extension mechanism — none of these are
hardcoded into an image, since most enterprise environments authenticate
through their own mechanism rather than a personal API key.

Cross-agent delegation is explicitly not supported for a sandboxed
agent yet (its loopback hub isn't reachable from inside a firewalled
container without extra plumbing) — see `sandbox/README.md`'s
out-of-scope section.

**Keep a sandboxed config alongside your normal one** rather than
editing `agents.yaml` in place — this repo ships three worked examples,
one per OS (`agents.yaml.sandbox.windows`, `.macos`, `.linux` — identical
except for volume-mount path syntax and whether the mounted gcloud
config directory comes from `{{ENV:APPDATA}}` or `{{ENV:HOME}}`; Claude/
opencode wrapped in `docker run`, Gemini left as a plain `spawn` with a
comment on which env vars to set), loaded via
`--agents=agents.yaml.sandbox.<os>` (see [Agent
registry](#agent-registry-agentsyaml)) instead of the default
`./chorus`.

## Headroom compression proxy (`agents.yaml`)

Opt-in integration with [Headroom](https://docs.headroomlabs.ai/docs), a
local compression proxy that sits between an agent CLI and its LLM
provider, shrinking tool outputs/logs/JSON/code before the model sees
them. chorus doesn't compress anything itself — enabling this starts (or
reuses) **one long-lived Docker container shared across every chorus run**,
not a fresh one per run, and each agent that opts in redirects its own
provider API calls through it via that agent's own base-URL environment
variable (e.g. `ANTHROPIC_BASE_URL` for Claude Code). Off by default.

The container is deliberately treated as persistent infrastructure, not
something chorus owns the lifecycle of end-to-end:
- **Reused, not recreated** — chorus checks whether `chorus-headroom` is
  already running before doing anything; if so, no `docker` command runs
  at all. If it exists but is stopped, chorus runs `docker start`
  (preserving its restart policy/volume/port); only a container that's
  never existed gets a fresh `docker run`.
- **`--restart unless-stopped`** — survives a Docker engine/Desktop
  restart on its own.
- **A named volume** (`chorus-headroom-data`, mounted at the image's own
  state directory) persists its savings/cache data across recreation.
- **chorus never stops it** — not on `quit`, not on Ctrl+C, not on a
  crash. It's meant to keep running (and keep its provider-side prompt
  cache warm) independently of any one chorus session. Stop it yourself
  with `docker rm -f chorus-headroom` if you ever want to.

```yaml
headroom:
  enabled: true
  # image: ghcr.io/headroomlabs-ai/headroom:code-nonroot   # this is the default
  # port: 8787                                               # optional override
  # mode: cache                                              # cache | token
  # output_shaper: "1"                                       # HEADROOM_OUTPUT_SHAPER (default "1")
  # output_holdout: "0.1"                                    # HEADROOM_OUTPUT_HOLDOUT (default "0.1")
```

This is the **containerized** Headroom image (`ghcr.io/headroomlabs-ai/headroom`)
— not a bare `headroom` binary on your PATH, nothing else to install.
Before any agent is spawned, chorus sets two environment variables in
its own process for `agents.yaml` to reference via chorus's existing
`{{ENV:NAME}}` substitution:

**Default image tag is `code-nonroot`, not `latest`.** Headroom
publishes an 8-way tag matrix — every combination of the `code`, `slim`,
and `nonroot` build modifiers; `latest` is the variant with *none* of
them. `code` adds Tree-sitter AST-aware code compression (the capability
that actually matters here, since chorus's agents' tool output is
overwhelmingly source code/diffs — `latest` silently compresses code
with the generic statistical path instead). `nonroot` runs as uid 1000
instead of root. **`slim` is deliberately NOT used**, despite being the
more locked-down option in principle (a distroless base with no shell/
curl/package manager) — **confirmed live** (2026-09, real `docker run`
on Rancher Desktop/Windows, WSL2 linux/amd64 backend): every `slim`-
tagged variant (`code-slim-nonroot` AND plain `slim-nonroot`, isolating
it to the distroless base itself, not the `code` extra) segfaults on
startup with zero log output, while `code-nonroot` (non-slim) started
cleanly and served real compressed requests in the same test. This may
be specific to this environment/backend — worth re-testing on your own
machine before reconsidering. `code-nonroot` publishes multi-arch
(amd64+arm64) same as every other variant. Override `image:` if you'd
rather use a different one (or a self-hosted mirror).

- `CHORUS_HEADROOM_HOST_URL` — reachable from a normal, non-sandboxed
  agent subprocess (`http://127.0.0.1:{port}`).
- `CHORUS_HEADROOM_SANDBOX_URL` — reachable from a **sandboxed** agent's
  own `docker run` container (`http://host.docker.internal:{port}`).

chorus never guesses which base-URL variable name a given agent's CLI
actually honors — same "confirm the real value yourself" discipline as
[Auto mode](#auto-mode-acp-session-modes)'s ACP session modes. You wire
the mapping explicitly, once, per agent:

**Non-sandboxed** — a new per-agent `env:` map (substituted the same way
`spawn`'s argv already is):

```yaml
- name: claude
  spawn: ["npx", "-y", "@agentclientprotocol/claude-agent-acp"]
  env:
    ANTHROPIC_BASE_URL: "{{ENV:CHORUS_HEADROOM_HOST_URL}}"
```

`env:` only reaches a bare host-process spawn — it sets environment
variables on the subprocess chorus itself execs. It does nothing for a
sandboxed `docker run` entry, since that subprocess IS the `docker` CLI,
not whatever ends up running inside the container it starts.

**Sandboxed** — add an explicit `-e` flag to the `docker run` argv
instead, the same way `agents.yaml.sandbox.*` already does for
`CHORUS_SANDBOX_ALLOW_HOSTS`:

```yaml
- name: claude
  spawn: ["docker", "run", "--rm", "-i",
          "-e", "ANTHROPIC_BASE_URL={{ENV:CHORUS_HEADROOM_SANDBOX_URL}}",
          ...]
```

### Rancher Desktop / `host.docker.internal` assumption

The sandboxed URL assumes **Rancher Desktop** (confirmed by [its own
FAQ](https://docs.rancherdesktop.io/faq/#q-can-containers-reach-back-to-host-services-via-hostdockerinternal))
resolves `host.docker.internal` inside a container with no extra flags —
notably, do NOT add `--add-host=host.docker.internal:host-gateway`: that
flag's `host-gateway` value is a Docker-Desktop-only feature Rancher
Desktop [doesn't support](https://docs.rancherdesktop.io/faq/#q-can-i-map-hostdockerinternal-or-hostrancher-desktopinternal-to-host-gateway-with-the-flag---add-host)
and adding it will error. A sandboxed agent's own egress firewall
(`sandbox/*/init-firewall.sh`) already allows traffic to/from the whole
host network subnet it auto-detects, so no `CHORUS_SANDBOX_ALLOW_HOSTS`
change is needed for this specifically.

**Security note**: the Headroom container's port is published on every
host interface (`-p {port}:8787`, not loopback-only) — not an oversight.
On Linux, Rancher Desktop's containers reach the host over a real Docker
bridge interface, which does NOT reach a `127.0.0.1`-only-bound host
port (unlike Docker Desktop's macOS/Windows VM-proxied networking,
which typically does) — binding loopback-only would silently break
sandboxed-agent reachability specifically on Linux. The tradeoff: the
proxy is reachable from any local process (and, depending on Rancher
Desktop's VM networking, possibly your LAN) while chorus is running.
This whole reachability picture (host.docker.internal → a broadly-bound
port, across all three OSes Rancher Desktop supports) is unverified
live — confirm on your own machine before relying on it, and see [Known
limitations](#known-limitations).

### What actually gets forwarded

Headroom's own docs describe passing provider API keys
(`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, AWS/Google credentials) to it for
more accurate token-count-based stats. chorus passes these through to
the container defensively (bare `-e NAME`, pulled from chorus's own
process environment if set, omitted if not — the same convention
`agents.yaml.sandbox.*` already uses) but doesn't require any of them:
an already-authenticated client (e.g. Claude Code sending its own
subscription auth header) has that header forwarded upstream through the
proxy unchanged regardless of whether Headroom has its own copy of a
provider key.

## Releases & publishing

`.github/workflows/release.yml` builds and publishes everything on a
tag push matching `v*.*.*` (e.g. `git tag v0.2.0 && git push origin
v0.2.0`):

- **Binaries** for `linux`/`darwin`/`windows` × `amd64`/`arm64` (6
  archives total — cross-compiled from a single Linux runner, since
  chorus and its dependencies are pure Go with no cgo), packaged as
  `.tar.gz` (macOS/Linux) or `.zip` (Windows), plus this repo's own
  `agents.yaml` (the exact same file `//go:embed`s into the binary) and
  a `checksums.txt` covering all of it — all attached as downloadable
  assets on a GitHub Release the workflow creates automatically for the
  tag. This is what [Install](#install)'s `install.sh`/`install.ps1`
  download from, both for the binary itself and to seed
  `~/.chorus/agents.yaml` on first install.
- **Sandbox Docker images** (see [Sandboxing](#sandboxing-containers)),
  built and pushed to Docker Hub as `matamagu/chorus-claude-sandbox`,
  `matamagu/chorus-gemini-sandbox`, and `matamagu/chorus-opencode-sandbox`,
  each tagged both `latest` and the release version. Claude/opencode
  publish for `linux/amd64` + `linux/arm64`; Gemini's image is
  `linux/amd64` only, since it's built on Google's own sandbox base image
  and whether that base publishes an `arm64` manifest has never been
  confirmed.

**One-time setup before the Docker push half will work**: add
`DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` as repo secrets (Settings ->
Secrets and variables -> Actions). `DOCKERHUB_TOKEN` should be a Docker
Hub *access token* (Docker Hub -> Account Settings -> Security -> New
Access Token), not your account password. The binary/release half of
the workflow needs no extra setup — it only uses the default
`GITHUB_TOKEN`.

To re-run a release (e.g. Docker Hub credentials were wrong the first
time) without re-pushing the tag, use the workflow's `workflow_dispatch`
trigger from the Actions tab with the existing tag name — it has a
`push_docker` toggle to skip the Docker half if you only need to fix the
binary assets.

`chorus --version` prints the version/commit/build-date stamped in by
`-ldflags` at release build time (`dev`/`none`/`unknown` for a plain
`go build .` from source).

## Session persistence

Quit chorus and come back later with `--resume` — `claude:`/`opencode:`
prompts pick up where you left off, in the same directory, instead of
starting cold every time:

```
$ ./chorus
claude ready (session 07ab2684-...)
> claude: remember the deploy target is us-east-1
> quit

$ ./chorus --resume
claude resumed (session 07ab2684-...)
> claude: what's the deploy target again?
[claude] us-east-1
```

Every run starts fresh unless `--resume` is passed — this is a
deliberate default (2026-09, inverted from the original resume-by-
default/`--fresh`-to-opt-out behavior at the user's explicit request): a
fresh session every time is the safer, more predictable choice for most
usage, and resuming is the deliberate exception you ask for.

This uses ACP's own `session/load` — not something chorus invented —
so it only works for agents that advertise support for it (confirmed
live for Claude and opencode; Gemini untested, still blocked). A
resumed session replays its prior turns back onto your screen as
scrollback before you type anything new, so it's visible what got
carried over, not a silent assumption.

- All of this state lives centrally under `~/.chorus/projects/<slug>/`
  (as of 2026-09-03 — previously a per-project `./.chorus/`), where
  `<slug>` is your project directory's own name plus a short hash of its
  full path (so two different projects that happen to share a folder
  name, e.g. two unrelated repos both called "backend", never collide).
  Override the root with `CHORUS_HOME` if you want chorus's state
  somewhere other than your home directory. Session IDs are still scoped
  per-project underneath that shared root — ACP's own `session/load`
  requires the request's cwd to match the session's original cwd, so
  centralizing *where* the files live doesn't change *what* gets resumed
  where.
- Each agent's last session ID is saved to `sessions.json` in that
  directory (a session ID is a pointer into that agent's own history
  tied to your account/machine, not something to share). Only the main
  interactive session is ever saved; delegation sub-sessions never are.
- Each agent subprocess's raw stderr (its own debug/error output — e.g.
  a stack trace if it fails to authenticate) is captured to
  `logs/<agent>.stderr.log` in that same directory (truncated fresh each
  run), never printed to your terminal directly — chorus's TUI owns the
  screen exclusively, so a subprocess writing unexpectedly to its own
  stderr can't corrupt it. Check that file if an agent is behaving oddly
  and chorus's own warning message isn't detailed enough.
- Upgrading from a version that used a per-project `./.chorus/`
  directory: that old directory is no longer read and can be deleted —
  chorus starts fresh under `~/.chorus/` the first time it runs in that
  project (falling back to a new session is already the normal, safe
  behavior for a missing or stale session ID).
- Every run starts every agent with a fresh session **by default** —
  pass `--resume` to resume prior sessions instead where possible (2026-09
  — inverted from the original default-resume/`--fresh`-to-opt-out
  behavior, at the user's explicit request, since a fresh session is the
  safer, more predictable default for most usage). A session created by
  any run — with or without `--resume` — becomes what a later `--resume`
  run picks up; it's not a one-time opt-in.
- If a saved session ID turns out to be stale (observed live: Claude's
  agent occasionally returns `session/load: Resource not found` for an
  ID that worked before — cause not pinned down, possibly related to
  how the ACP adapter is invoked fresh via `npx` each run), chorus
  falls back to a normal new session automatically and clears the dead
  ID, rather than failing to start.

## Security

chorus went through a full security audit before its first public push.
What that means in practice:

- **The permission system is a real enforcement boundary, not a
  suggestion.** Every path that can read a file, write a file, or run
  a shell command — including chorus's own client-owned `terminal/create`/
  `fs/read_text_file`/`fs/write_text_file` RPCs, not just the
  agent-initiated ones you already saw prompts for — goes through
  `agents.yaml`'s allow-list, and asks interactively for anything not
  explicitly allowed. This wasn't always true: earlier versions
  executed the client-owned RPCs unconditionally, bypassing the policy
  file entirely for any agent that used them instead of its own
  internal tools. Fixed and unit-tested (`internal/acpclient`'s test
  suite exercises the auto-allow/ask/deny/UNC-rejection paths
  directly, since neither Claude nor opencode currently exercise these
  RPCs live — see the spec entry for why that made live verification
  impossible and what was done instead).
- **UNC paths (`\\host\share\...`) are rejected outright** on any file
  read/write chorus mediates — Windows auto-authenticates network
  paths with the current user's credentials, a known credential-theft
  technique that needs no code execution at all.
- **`agents.yaml` is trusted, executable-adjacent config** — see the
  warning under [Agent registry](#agent-registry-agentsyaml).
  This is the one class of risk chorus can't design away: if you run
  it against a config file, you're trusting that file the same way
  you'd trust a shell script.
- **Cross-agent delegation's loopback HTTP hub** (see
  [Cross-agent delegation](#cross-agent-delegation)) uses a random
  per-run token with constant-time comparison, a concurrency cap, and
  request size limits. Known, deliberately-accepted residual risk: the
  token is necessarily visible to each agent's own subprocess (it has
  to be, to reach the hub), so if that agent's own CLI logs its RPC
  parameters verbosely, the token could appear in that agent's own
  logs. This is inherent to how MCP server configs pass secrets
  generally, not a chorus-specific gap.
- Agent-supplied text is stripped of ANSI/terminal escape sequences
  before being printed, so a manipulated agent response can't hide or
  spoof what's shown in your terminal.

None of this changes the fundamental trust model: chorus runs with
your full user permissions, the same as any CLI tool you invoke
directly. The audit hardened the boundary between "what an agent asks
for" and "what actually happens without your say-so" — it doesn't (and
can't) protect you from an agent doing something harmful *after* you
approve it.

## Project layout

```
main.go                   REPL loop, routing/command dispatch, image-attachment
                            extraction, single-writer terminal ownership
internal/session/          Connection (subprocess + ACP handshake) and AgentSession
                            (one session on a Connection) — narrow Prompt/
                            PromptContent/Close API
internal/acpclient/        ACP Client role — receives updates/permission requests,
                            answers file read/write and terminal RPCs
internal/render/           Renders every session/update kind to the terminal,
                            including saving inbound images to disk
internal/policy/           Permission/delegation/compaction/routing config types +
                            AutoAllow/AutoAllowTool matching (agents.yaml)
internal/router/           LLM-based routing decision: BuildDecisionPrompt/
                            ParseDecision (keyword matching removed 2026-08-25)
internal/registry/         agents.yaml loading — the single decode point for
                            the whole file, registry + policy.Config together
internal/delegate/         Cross-agent delegation: the delegate-mcp subprocess
                            mode and the loopback Hub it calls back into
internal/sessionstore/     sessions.json persistence for session resume, under
                            ~/.chorus/projects/<slug>/ (see Session persistence)
internal/bus/               Shared message types between agent connections and main
sandbox/                   Opt-in per-agent container images for filesystem/network
                            containment — see Sandboxing above and sandbox/README.md
```

## Known limitations

- Terminal-capability output (for agents that execute shell commands
  via client-owned terminals) is captured but not streamed live to the
  renderer — only surfaced as "[terminal output omitted]" inside a
  tool call today.
- A `/init`-triggered shell pipeline was once seen failing with exit
  code 2 and misdiagnosed as a bug in `internal/acpclient.CreateTerminal`
  (assumed to exec commands without shell interpretation). **Debug
  instrumentation disproved this**: `CreateTerminal` was never called
  for that failure at all — Claude executes its own Bash tool calls
  inside its own subprocess, not via chorus's client-owned terminal
  RPC. The real failure happens entirely inside `claude-agent-acp`'s
  own process (likely a Windows-vs-Unix shell syntax mismatch,
  `2>/dev/null` meaning nothing to whatever shell it invokes here) —
  outside chorus's code and not something chorus can fix.
  `CreateTerminal`'s `Command`/`Args` handling itself (a plain
  exec-style pair per ACP's schema, not a shell command line) is
  unchanged; it *has* since gained a permission gate and
  output-byte-limit handling — see [Security](#security).
- Delegation's reply-collection has a theoretical (never observed)
  race on the very last streamed chunk of a sub-session's reply.
- No multi-hop delegation (by design, not a gap).
- **LLM-based routing (`routing.mode: llm`) is new and not yet
  live-verified** (2026-08-25): whether any of Claude/Gemini/opencode
  actually advertise a model-switch command at all is unconfirmed — the
  common case may turn out to be "agent only, no model switch," not the
  exception. The decision call also can't structurally prevent the
  decision agent from using its own built-in tools during what's meant
  to be a cheap classification turn (chorus can only ask it not to);
  a 25s timeout bounds the damage if that happens.
- Inbound image rendering (agent -> you) is covered by unit tests but
  hasn't been triggered by a real agent in practice — most coding
  tasks never make one send image content back.
- `Connection.SupportsImagePrompts` is recorded but unused — chorus
  doesn't yet warn if you attach an image to an agent that never
  advertised support for receiving one.
- The bubbletea TUI has been built and is covered by unit tests, but
  hasn't yet been run in a real terminal to confirm it actually looks
  and behaves right (scrolling, mouse wheel, spinner animation,
  terminal restoration on quit) — see `CLAUDE.md`'s Known open issues
  before assuming this is fully verified.
- **Esc-to-interrupt and click-drag copy-on-select (2026-09) are unit-
  tested only, not yet watched live** — same PTY-less tool-invocation-
  environment caveat as the rest of the bubbletea TUI above. Worth
  specifically confirming on a real terminal: `Esc` actually stops a
  real in-flight agent turn rather than just sending the notification
  and having the agent ignore it (`session/cancel` behavior is entirely
  up to the agent — ACP doesn't guarantee an early stop, just that the
  request was sent), and that a click-drag selection lines up with what
  you'd visually expect to have highlighted, including across a
  scrolled/streaming viewport.
- **No checkpointing/rewind** (restore code/conversation/both to an
  earlier turn) — deliberately out of scope for this pass, not an
  oversight. It's a materially larger feature (durable per-turn
  snapshots, a restore UI, deciding what "restore" even means across N
  independently-running agent sessions rather than one) than the other
  gaps closed alongside it; worth a dedicated design pass of its own if
  it turns out to matter to real usage.
- **Sandboxing (2026-08-26/27): confirmed working end-to-end for
  Claude, Gemini, and opencode's free-tier backend, on a real machine,
  with real ACP handshakes — not just the underlying mechanism.** A live
  run of `./chorus --agents=agents.yaml.sandbox.<os>` completed real
  `initialize` handshakes for all three sandboxed agents (`commands`
  showed each one's actual advertised slash-command list) and a real
  prompt against sandboxed opencode completed successfully. Getting here
  found and fixed four real bugs along the way (none guessed — each
  root-caused with live evidence first): a Claude base-image Node
  version mismatch; a stdout-corrupting firewall-script bug that would
  have broken every sandboxed agent's handshake unconditionally; Gemini
  CLI's own `GEMINI_SANDBOX=docker` sandboxing being fundamentally
  incompatible with `--acp` mode (fixed by wrapping Google's real
  sandbox image directly instead); and an `EROFS` crash from mounting
  Gemini's OAuth credential directory read-only (fixed by mounting it
  read-write instead — a deliberate, documented tradeoff). opencode's
  free-tier backend needed one more fix: its firewall shipped with no
  LLM backend domain baked in until the real one (`opencode.ai`) was
  confirmed live and added. **Resolved (2026-09)**: opencode's VPN-bound
  Ollama endpoint reachability — the actual fix was making
  `init-firewall.sh`'s DNS resolution non-strict for
  `CHORUS_SANDBOX_ALLOW_HOSTS` entries, since the earlier hard failure
  on an unresolved host was a startup-timing race against the VPN
  coming up, not a routing problem — see `sandbox/opencode/README.md`.
- **Headroom compression proxy integration (2026-09): confirmed live for
  Claude on Windows/Rancher Desktop** — see [Headroom compression
  proxy](#headroom-compression-proxy-agentsyaml). A real
  `session.Connect`/`NewSession`/`Prompt` round trip against the actual
  `claude-agent-acp` subprocess, redirected via `ANTHROPIC_BASE_URL` at
  a real `internal/headroom`-managed container, produced a real reply AND
  showed up in Headroom's own `/stats` as genuine compressed Anthropic
  traffic (`"agent":"claude-code"`, real token counts, a real prompt-cache
  hit on a second run) — so **the ACP adapter chorus actually spawns does
  honor `ANTHROPIC_BASE_URL`**, not just the bare CLI. Also confirmed:
  `host.docker.internal` from a separate container reaches the published
  port on this setup (Rancher Desktop/Windows, WSL2 backend). Still
  unconfirmed: the same `host.docker.internal` path on Rancher Desktop's
  native-Linux backend specifically (a materially different network path
  — see `internal/headroom`'s own doc comment); and whether opencode's
  own multi-provider config accepts a custom base URL the same way.
  Gemini CLI is lowest priority here regardless, since it's already
  blocked entirely by the free-tier `IneligibleTierError` above.
- **`session.Connection.Close()` may not kill an `npx`-spawned agent's
  actual Node.js process on Windows** — found live (2026-09) while
  testing the above: after `Close()`, the spawned subprocess's own cwd
  stayed locked (a file couldn't be deleted, "used by another process")
  for up to several seconds, consistent with `cmd.Process.Kill()` only
  reaching the immediate `npx` wrapper, not the Node.js process `npx`
  spawns underneath it. Not Headroom-specific — this affects any
  `npx`-based `spawn` entry (the default `claude` entry included) on
  Windows, and could mean a `quit`/Ctrl+C in chorus itself leaves an
  orphaned Node process behind rather than a leaked file lock. Worth a
  dedicated fix (likely: track and kill the actual process tree, or run
  `npx` via a mechanism that doesn't fork further) — not yet
  investigated beyond confirming the symptom.
