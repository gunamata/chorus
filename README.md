# chorus

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

Full design rationale lives in [`chorus-spec.md`](chorus-spec.md). This
file is the practical "how do I run it" doc.

## Status

v1 (core plumbing), v2 (auto-routing + registry), v3 (cross-agent
delegation), session persistence, slash-command passthrough, and image
attachments (both directions) are all built and verified live against
real Claude and real opencode. See
[`chorus-spec.md` §0](chorus-spec.md#0-context-for-whoever-implements-this-read-first)
for the full verified-findings log — in particular, **Gemini's ACP
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
  rejected by `gemini --acp` itself (see chorus-spec.md §0) — this is
  a Google-side restriction, not a chorus bug or a login problem.
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
silently reject unsigned local binaries. See chorus-spec.md §0 for
what this looks like when it happens.

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

## Build

```sh
go build -o chorus.exe .
```

## Run

```sh
./chorus.exe                                      # every agent starts a fresh session
./chorus.exe --resume                             # resume prior sessions where possible instead
./chorus.exe --agents=agents.yaml.sandbox.windows # use a different agents.yaml than the default
```

On startup chorus spawns all three agent subprocesses and creates (or,
with `--resume` — see [Session persistence](#session-persistence) —
resumes) an ACP session with each. If one fails to start (e.g. Gemini's
tier restriction), chorus prints a warning and continues with whichever
agent(s) started successfully.

### Usage

chorus runs as a real terminal UI (via
[bubbletea](https://github.com/charmbracelet/bubbletea)): a scrolling
output pane above a fixed input box at the bottom, alt-screen just like
Claude Code / Gemini CLI / opencode / Codex — not a plain scroll-by
REPL. The interaction itself is unchanged, just the display:

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

That first line — `[claude] fix the login bug in auth.py` — is chorus echoing your own submitted prompt back into the scrollback, tagged by which agent it went to, on a solid highlighted background (matching Claude Code's own convention for setting your input apart from the reply — not visible in this plain-text example, but real in the actual TUI). ACP has no way to do this for you on a live turn (ask returns the reply, not an echo of what was asked), so chorus does it explicitly — without it, a long session would give you replies with no record of which question each one answers.

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
  leaving that dead, chorus implements copy-on-select itself, the same
  approach Claude Code CLI's own fullscreen UI takes: dragging over one
  or more lines highlights them live as you drag (whole lines, not
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
  scrolling sideways forever. Press **ctrl+j** for a new line within the
  same prompt (Enter always submits). `Home`/`End` (and, for the same
  job, `ctrl+a`/`ctrl+e` — macOS Terminal/iTerm's own readline-style
  bindings) jump to the start/end of the current line. `↑`/`↓` move the
  cursor between lines like any multi-line editor — and once the cursor
  is already on the topmost or bottommost line, they instead walk
  through **prompt history** (everything you've submitted this session,
  most recent first), the same way a shell's `↑` does.
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
  busy; otherwise every currently-busy agent), so a wrong or overly
  long request doesn't force you to kill the whole program the way
  `ctrl+c` does. The busy-status line above the input box shows
  "(esc to interrupt)" whenever it's actually actionable.
- `quit` / `exit` / `ctrl+c` cleanly end every session, kill the
  subprocesses, and restore your terminal (leaves the alt-screen,
  cursor visible). Unlike `Esc`, this ends every agent's session at once
  — reach for `Esc` first if you only want to stop one running turn.
- Full reasoning/thinking text is **hidden by default** — while an agent
  is thinking you see a brief `[agent] ⠋ Pondering…`-style indicator (a
  random word from a small set, spinner-animated, the same idea as
  Claude Code/Gemini CLI/Codex's own rotating-word indicators) instead
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
  guesswork.

Agent replies and thoughts are rendered as styled markdown (bold,
headers, code blocks — via [glamour](https://github.com/charmbracelet/glamour)),
redrawn in place as each chunk streams in. In-progress tool calls show
an animated spinner. See chorus-spec.md §15b for how the TUI is
implemented (§15a has the earlier, now-superseded plain-terminal
version of this same markdown/spinner work).

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
model.

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
./chorus.exe                                      # local -> central -> embedded, first match wins
./chorus.exe --agents=agents.yaml.sandbox.windows # loads that local file explicitly instead
./chorus.exe --agents=https://gist.githubusercontent.com/you/id/raw/agents.yaml # or fetch one remotely
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
`policy.yaml`) is the §5 allow-list, keyed on ACP's standardized
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
into Claude Code's own usage patterns — compacting around 60% produces
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
  delegation, not just a theoretical gap — see chorus-spec.md §0.)
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
sockets" — see `chorus-spec.md` §0/§2 for why it's unavoidable given
how ACP's `mcpServers` mechanism actually works.

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
`./chorus.exe`.

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
$ ./chorus.exe
claude ready (session 07ab2684-...)
> claude: remember the deploy target is us-east-1
> quit

$ ./chorus.exe --resume
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

chorus went through a full security audit (chorus-spec.md §0's
2026-08-22 entry has the complete findings list) before its first
public push. What that means in practice:

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
                            AutoAllow/AutoAllowTool matching (agents.yaml, §5)
internal/router/           LLM-based routing decision: BuildDecisionPrompt/
                            ParseDecision (§9 — keyword matching removed 2026-08-25)
internal/registry/         agents.yaml loading (§10) — the single decode point for
                            the whole file, registry + policy.Config together
internal/delegate/         Cross-agent delegation (§11): the delegate-mcp subprocess
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
  outside chorus's code and not something chorus can fix. See
  `chorus-spec.md` §0 for the full account. `CreateTerminal`'s
  `Command`/`Args` handling itself (a plain exec-style pair per ACP's
  schema, not a shell command line) is unchanged; it *has* since
  gained a permission gate and output-byte-limit handling — see
  [Security](#security).
- Delegation's reply-collection has a theoretical (never observed)
  race on the very last streamed chunk of a sub-session's reply — see
  `chorus-spec.md` §0 for details.
- No multi-hop delegation (by design, not a gap — see §2's non-goals).
- **LLM-based routing (`routing.mode: llm`) is new and not yet
  live-verified** (2026-08-25): whether any of Claude/Gemini/opencode
  actually advertise a model-switch command at all is unconfirmed — the
  common case may turn out to be "agent only, no model switch," not the
  exception. The decision call also can't structurally prevent the
  decision agent from using its own built-in tools during what's meant
  to be a cheap classification turn (chorus can only ask it not to);
  a 25s timeout bounds the damage if that happens. See `chorus-spec.md`
  §0 for the full account and what to check first on a real run.
- Inbound image rendering (agent -> you) is covered by unit tests but
  hasn't been triggered by a real agent in practice — most coding
  tasks never make one send image content back.
- `Connection.SupportsImagePrompts` is recorded but unused — chorus
  doesn't yet warn if you attach an image to an agent that never
  advertised support for receiving one.
- The bubbletea TUI (§15b) has been built and is covered by unit tests,
  but hasn't yet been run in a real terminal to confirm it actually
  looks and behaves right (scrolling, mouse wheel, spinner animation,
  terminal restoration on quit) — see `chorus-spec.md` §0's most recent
  entry and `CLAUDE.md`'s Known open issues before assuming this is
  fully verified.
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
- **No checkpointing/rewind** (Claude Code CLI's `/rewind`, double-Esc,
  restore code/conversation/both) — deliberately out of scope for this
  pass, not an oversight. It's a materially larger feature (durable
  per-turn snapshots, a restore UI, deciding what "restore" even means
  across N independently-running agent sessions rather than one) than
  the other gaps closed alongside it; worth a dedicated design pass of
  its own if it turns out to matter to real usage.
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
  LLM backend domain baked in until the real one
  (`opencode.ai`) was confirmed live and added. See `chorus-spec.md`
  §0's 2026-08-26/27 entries for the full diagnostic trail. **Resolved
  (2026-09)**: opencode's VPN-bound Ollama endpoint reachability — the
  actual fix was making `init-firewall.sh`'s DNS resolution non-strict
  for `CHORUS_SANDBOX_ALLOW_HOSTS` entries, since the earlier hard
  failure on an unresolved host was a startup-timing race against the
  VPN coming up, not a routing problem — see `sandbox/opencode/README.md`
  and `chorus-spec.md` §0's 2026-09-02 entry.
