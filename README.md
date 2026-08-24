# chorus

A single foreground CLI that owns several ACP agent sessions — Claude
Code, Gemini CLI, and opencode. Type commands, they route to whichever
agent you name, output streams back live, and permission questions
surface as a normal prompt in the same terminal. Quit and come back
later — see [Session persistence](#session-persistence) — and
supported agents pick up where you left off instead of starting cold.

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

## Build

```sh
go build -o chorus.exe .
```

## Run

```sh
./chorus.exe          # resumes prior sessions where possible
./chorus.exe --fresh  # start every agent clean, ignoring saved sessions
```

On startup chorus spawns all three agent subprocesses and creates (or
resumes — see [Session persistence](#session-persistence)) an ACP
session with each. If one fails to start (e.g. Gemini's tier
restriction), chorus prints a warning and continues with whichever
agent(s) started successfully.

### Usage

chorus runs as a real terminal UI (via
[bubbletea](https://github.com/charmbracelet/bubbletea)): a scrolling
output pane above a fixed input box at the bottom, alt-screen just like
Claude Code / Gemini CLI / opencode / Codex — not a plain scroll-by
REPL. The interaction itself is unchanged, just the display:

```
[claude] Reading auth.py...
[claude] ● Read auth.py (completed)

PERMISSION: claude wants to: Edit auth.py
  1) Deny (reject_once)
  2) Allow Once (allow_once)
  3) Always Allow (allow_always)
[claude] Fixed the null check on line 42.
──────────────────────────────────────────────
> _
```

- Type a prompt with no prefix and chorus **auto-routes** it (see
  [Routing](#routing-policyyaml) below). `<agent>: <text>` always
  overrides the router and sends straight to that agent's persistent
  session (`claude`, `gemini`, or `opencode`).
- All started agents run concurrently — output from any of them can
  appear at any time, including mid-turn, interleaved in the same
  scrolling pane.
- **Scroll the output pane** with `PgUp`/`PgDn`/`Home`/`End` (also
  `ctrl+u`/`ctrl+d` for half-page steps) — it auto-follows new output
  while you're at the bottom, and stays put (doesn't get yanked back
  down) if you've scrolled up to read something while agents keep
  streaming. Mouse wheel scrolling is deliberately not enabled — it
  requires capturing all mouse input, which breaks your terminal's
  native click-drag text selection/copy, and that matters more for a
  coding tool.
- Permission prompts render whatever option set the agent actually
  sent (never a fixed menu). Answer with **arrow keys (↑/↓) + Enter**,
  or by typing the option's number, its name/kind (case-insensitive), or
  `cancel` — both work interchangeably, even in the same prompt. The
  routeAsk agent-choice menu (ambiguous auto-routing, or an ambiguous
  slash command) works the same way.
- `quit` / `exit` / `ctrl+c` cleanly end every session, kill the
  subprocesses, and restore your terminal (leaves the alt-screen,
  cursor visible).
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

If an agent sends an image back, it's saved under `.chorus/images/`
and the path is printed — a terminal can't display the bytes inline,
but a path you can open beats a bare `[image]` placeholder.

## Agent registry (`agents.yaml`)

Which agents chorus spawns, and how, lives in data, not code:

```yaml
agents:
  - name: claude
    spawn: ["npx", "-y", "@agentclientprotocol/claude-agent-acp"]
    cost_tier: metered
    notes: "general-purpose, metered usage"
    transport: acp
  - name: gemini
    spawn: ["gemini", "--acp"]
    cost_tier: seat
    notes: "seat/subscription usage"
    transport: acp
  - name: opencode
    spawn: ["opencode", "acp"]
    cost_tier: free
    notes: "free by default, zero configured credentials"
    transport: acp
```

Add an agent by adding an entry — nothing else needs to change.
`transport` must be `acp` (the only one chorus implements). Entries
are tried in file order at startup; one failing (e.g. Gemini's tier
block) doesn't stop the others. `cost_tier` and `notes` are free text
you edit to describe your own setup — they're now surfaced to every
OTHER connected agent via the `delegate` tool's description and a
seeded first-turn briefing, so an agent has concrete signal for
deciding whether and to whom to delegate a sub-task (see
[Cross-agent delegation](#cross-agent-delegation)).

**`agents.yaml` is trusted, executable configuration, not passive
data** — `spawn` is a literal command line chorus runs unconditionally
at startup. Never point chorus at an `agents.yaml` (or `policy.yaml`)
you didn't write or don't fully trust; using someone else's is
equivalent to running a script they handed you. Same trust model as a
Makefile or a VS Code `tasks.json`.

## Routing (`policy.yaml`)

A prompt with no `agent:` prefix is auto-routed by keyword, cheaply —
this is whole-word matching (not a substring, and not a model call,
since burning tokens on a classifier would defeat the point of routing
cheap tasks away from an expensive agent). Whole-word matters: an
earlier version matched substrings and mis-routed on "prefix"/"suffix"
(false-matching "fix") and "checklist" (false-matching "list") —
see `internal/router`'s tests.

```yaml
routing:
  default: opencode
  ask_when_ambiguous: false
  rules:
    - match: [fix, implement, refactor, debug, architecture, bug, error]
      agent: claude
    - match: [summarize, explain, list, scan, review, analyze]
      agent: gemini
```

First matching rule wins; an unmatched prompt falls back to `default`.
Set `ask_when_ambiguous: true` to have chorus ask which agent should
handle an unmatched prompt instead of guessing via `default`.

## Permission policy (`policy.yaml`)

The same file also holds the §5 allow-list, keyed on ACP's
standardized `ToolCallUpdate.Kind` (`read`, `edit`, `delete`, `move`,
`search`, `execute`, `think`, `fetch`, `switch_mode`, `other`) since
that's the one thing every agent reports uniformly — not on per-agent
tool names, which aren't visible over ACP:

```yaml
claude:
  auto_allow: [read, search, think]
  auto_allow_tools: [mcp__chorus-delegate__]
gemini:
  auto_allow: [read, search, think]
  auto_allow_tools: [mcp__chorus-delegate__]
opencode:
  auto_allow: [read, search, think]
  auto_allow_tools: [mcp__chorus-delegate__]
```

`auto_allow_tools` matches by the tool call's title (case-insensitive
**prefix**, not substring — see [Security](#security) for why) instead
of kind — needed for chorus's own `delegate` tool below, since an
external MCP tool's kind is generically `other` to every agent, so
kind-based matching can't single it out.

Anything not listed under either falls through and asks. Missing
`policy.yaml` is not an error — everything just asks.

**This allow-list is enforced, not advisory**: `edit`/`execute` (and
everything else not explicitly listed) require your approval even for
agents that route file writes and shell commands through chorus's own
client-owned RPCs rather than doing it internally — see
[Security](#security).

## Cross-agent delegation

With 2+ agents running, every agent's session gets a `delegate(agent,
task)` tool: one agent can hand a self-contained sub-task to another
and get back its text reply, instead of you manually copy-pasting
between them. Example: ask Claude to delegate a summarization task to
opencode, and Claude will call the tool itself if it decides to.

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
  model has concrete signal to weigh, not just bare agent names.
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
- Disable the briefing with `policy.yaml`:
  ```yaml
  delegation:
    briefing: false
  ```
  (On by default — delegation is meant to be something an agent
  reaches for on its own, not something you have to ask for every
  time.)
- **Known limitation**: if a session already existed before a 2nd
  agent became available (e.g. you ran chorus with just Claude, then
  later added opencode to `agents.yaml`), that pre-existing session
  never gets the briefing — restart with `--fresh` to pick it up.

This works by chorus re-invoking itself as a small local MCP server
(`chorus __mcp_delegate`) that each agent's own `mcpServers` config
spawns, which calls back into chorus's main process over a
127.0.0.1-only HTTP listener (random port and per-run token, never
touches the network). This is a deliberate, narrow exception to "no
sockets" — see `chorus-spec.md` §0/§2 for why it's unavoidable given
how ACP's `mcpServers` mechanism actually works.

## Session persistence

Quit chorus and come back later — `claude:`/`opencode:` prompts pick up
where you left off, in the same directory, instead of starting cold
every time:

```
$ ./chorus.exe
claude ready (session 07ab2684-...)
> claude: remember the deploy target is us-east-1
> quit

$ ./chorus.exe
claude resumed (session 07ab2684-...)
> claude: what's the deploy target again?
[claude] us-east-1
```

This uses ACP's own `session/load` — not something chorus invented —
so it only works for agents that advertise support for it (confirmed
live for Claude and opencode; Gemini untested, still blocked). A
resumed session replays its prior turns back onto your screen as
scrollback before you type anything new, so it's visible what got
carried over, not a silent assumption.

- Each agent's last session ID is saved to `.chorus/sessions.json`
  (project-local — gitignored, since a session ID is a pointer into
  that agent's own history tied to your account/machine, not something
  to share). Only the main interactive session is ever saved;
  delegation sub-sessions never are.
- Each agent subprocess's raw stderr (its own debug/error output — e.g.
  a stack trace if it fails to authenticate) is captured to
  `.chorus/logs/<agent>.stderr.log` (truncated fresh each run), never
  printed to your terminal directly — chorus's TUI owns the screen
  exclusively, so a subprocess writing unexpectedly to its own stderr
  can't corrupt it. Check that file if an agent is behaving oddly and
  chorus's own warning message isn't detailed enough.
- `./chorus.exe --fresh` skips resuming and starts every agent clean —
  the new session then becomes what gets resumed next time, not a
  permanent opt-out.
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
  `policy.yaml`'s allow-list, and asks interactively for anything not
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
- **`agents.yaml`/`policy.yaml` are trusted, executable-adjacent
  config** — see the warning under [Agent registry](#agent-registry-agentsyaml).
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
internal/policy/           policy.yaml loading — auto-allow checks + routing config
internal/router/           Keyword-based auto-routing (§9)
internal/registry/         agents.yaml loading (§10)
internal/delegate/         Cross-agent delegation (§11): the delegate-mcp subprocess
                            mode and the loopback Hub it calls back into
internal/sessionstore/     .chorus/sessions.json persistence for session resume
internal/bus/               Shared message types between agent connections and main
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
