package tui

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	acp "github.com/coder/acp-go-sdk"

	"chorus/internal/bus"
	"chorus/internal/delegate"
	"chorus/internal/policy"
	"chorus/internal/render"
	"chorus/internal/router"
	"chorus/internal/session"
)

// AgentWorker serializes prompts to one AgentSession: a line handed to
// dispatch() is queued here rather than calling Prompt inline, because
// Prompt blocks until the agent's whole turn completes and the Model's
// Update loop must stay free to keep handling outputCh/permCh messages
// while that happens (chorus-spec.md §7). Queued as content blocks, not
// plain text, so an @-attached image can ride alongside the text in the
// same turn.
type AgentWorker struct {
	sess *session.AgentSession
	in   chan []acp.ContentBlock
	// busy is true while a prompt is in flight (between dequeuing from in
	// and PromptContent returning) — read via Idle() by
	// acpclient.Client's delegation-nudge logic (CLAUDE.md's delegation-
	// maximization plan item 2) to decide whether a cheaper agent is
	// actually free to take a delegated sub-task right now, not just
	// connected. atomic rather than a mutex: the only operations are a
	// single bool set/read, and Idle() is called from a different agent's
	// connection read-loop goroutine, not this worker's own.
	busy atomic.Bool
	// startedAt records when the current in-flight prompt was dequeued —
	// read by Model's View() (via StartedAt) every render to show a
	// persistent "<agent> working (12s)" status line, since without it a
	// long-running turn with no streamed output yet (an agent silently
	// "thinking" before its first token) looked identical to nothing
	// happening at all. atomic.Value rather than a plain field for the
	// same cross-goroutine-read reason busy is atomic; set right alongside
	// busy so the two never observably disagree.
	startedAt atomic.Value // time.Time
}

// Idle reports whether w currently has no prompt in flight.
func (w *AgentWorker) Idle() bool {
	return !w.busy.Load()
}

// Cancel requests the agent stop its current in-flight prompt, if any —
// backs Esc's interrupt-one-turn behavior (Model.interruptBusyAgents),
// chorus's answer to the single biggest gap found against Claude Code
// CLI's own Esc-to-interrupt: previously the only way to stop a running
// turn was Ctrl+C, which kills every connected agent's subprocess at
// once. Sends ACP's session/cancel notification and returns immediately —
// it does not itself wait for the in-flight PromptContent call to return;
// StartWorker's goroutine still owns that, and reports the (typically
// early-terminated, not erroring) result via doneCh/errCh exactly as for
// any other completed prompt. Safe to call on an idle worker (the
// notification is fire-and-forget; an agent with nothing in flight is
// expected to just ignore it) — callers should still prefer checking
// Idle() first so "interrupted" isn't printed for an agent that wasn't
// doing anything.
func (w *AgentWorker) Cancel(ctx context.Context) error {
	return w.sess.Cancel(ctx)
}

// StartedAt reports when w's current in-flight prompt began, if it's busy
// right now. The second return is false while idle — callers must check
// Idle() (or this) before trusting the time, since startedAt isn't cleared
// on completion, only overwritten by the next prompt.
func (w *AgentWorker) StartedAt() (time.Time, bool) {
	if w.Idle() {
		return time.Time{}, false
	}
	t, ok := w.startedAt.Load().(time.Time)
	return t, ok
}

// ErrMsg is a per-prompt error tagged with which agent it came from.
type ErrMsg struct {
	Agent string
	Err   error
}

// PromptDoneMsg reports that agent's in-flight prompt finished (success or
// error — Err is nil on success), and how long it took. Sent unconditionally
// by StartWorker's goroutine so Model can append a "finished in Xs" line
// regardless of outcome; exported (like ErrMsg) since main.go constructs
// the channel it travels on.
type PromptDoneMsg struct {
	Agent    string
	Duration time.Duration
	Err      error
}

// formatDuration renders d as a short, human-scaled duration — "45s",
// "2m14s", "1h05m" — used both for PromptDoneMsg's "finished in ..." line
// and the persistent "<agent> working (...)" status line, so a long-running
// turn reads the same way whether you're watching it live or saw it after
// the fact.
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// StartWorker launches the goroutine that drains w.in and calls
// s.PromptContent for each queued prompt, reporting any error on errCh and,
// once the turn finishes either way, its duration on doneCh — Model uses
// that to append a "finished in Xs" line, since otherwise a completed turn
// looked no different in the document from one still running (nothing
// resembling "done" was ever printed at all). Called from main.go's startup
// phase 3, before the Model exists — the returned worker is handed to New
// via Config.Workers.
func StartWorker(ctx context.Context, s *session.AgentSession, errCh chan<- ErrMsg, doneCh chan<- PromptDoneMsg) *AgentWorker {
	w := &AgentWorker{sess: s, in: make(chan []acp.ContentBlock, 16)}
	go func() {
		for blocks := range w.in {
			start := time.Now()
			w.startedAt.Store(start)
			w.busy.Store(true)
			err := s.PromptContent(ctx, blocks)
			w.busy.Store(false)
			if err != nil {
				errCh <- ErrMsg{Agent: s.Name, Err: err}
			}
			doneCh <- PromptDoneMsg{Agent: s.Name, Duration: time.Since(start), Err: err}
		}
	}()
	return w
}

// nativeCommandTimeout bounds a "!"-command's run — without this, a
// command that waits on stdin (chorus never connects one — see
// shellCommand) or otherwise hangs would tie up its goroutine forever with
// no feedback and no way to cancel it (bubbletea's ctrl+c quits the whole
// program, it doesn't reach into a running tea.Cmd).
const nativeCommandTimeout = 5 * time.Minute

// nativeCmdResultMsg reports a completed "!"-command — see
// runNativeCommand's doc comment.
type nativeCmdResultMsg struct {
	cmdline  string
	output   string
	err      error
	duration time.Duration
}

// shellCommand returns an *exec.Cmd that runs line through the platform's
// shell (cmd /C on Windows, sh -c elsewhere) rather than as a single bare
// argv, so a "!"-command supports the pipes/redirects/&&-chains a user
// would expect from a real terminal. Stdin is left unset (nil), i.e.
// connected to /dev/null-equivalent, deliberately — bubbletea owns the
// real stdin once the program is running (concurrency invariant #2,
// CLAUDE.md), so a command that tries to read from it would just hang
// until nativeCommandTimeout kills it, not silently steal keystrokes from
// the TUI.
func shellCommand(ctx context.Context, line string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", line)
	}
	return exec.CommandContext(ctx, "sh", "-c", line)
}

// runNativeCommand executes cmdline directly via the OS shell, bypassing
// every connected agent entirely — chorus's answer to "deterministic,
// zero-reasoning work (go test, gofmt -l, a build) shouldn't spend an LLM
// turn, metered or free, just to run a command with one already-correct
// answer" (CLAUDE.md's delegation-maximization plan, item #1). Runs as a
// tea.Cmd (bubbletea's own async pattern for one-off work) rather than
// through an AgentWorker's queue — there's no agent turn to serialize
// against, and Update() must stay non-blocking exactly as it does for a
// real agent prompt (concurrency invariant #1). ctx is expected to be
// Model.ctx, wrapped here with nativeCommandTimeout so a hung command
// can't block forever with no feedback.
func runNativeCommand(ctx context.Context, cmdline string) tea.Cmd {
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, nativeCommandTimeout)
		defer cancel()
		start := time.Now()
		out, err := shellCommand(cctx, cmdline).CombinedOutput()
		return nativeCmdResultMsg{cmdline: cmdline, output: string(out), err: err, duration: time.Since(start)}
	}
}

// pendingRoute is a prompt awaiting a user choice of agent — either §9's
// ask_when_ambiguous (candidates == nil: offer every known agent) or an
// ambiguous slash command declared by more than one agent (candidates:
// just those).
type pendingRoute struct {
	text       string
	candidates []string
}

// dispatch handles one input line, exactly as chorus-spec.md's REPL
// syntax defines it:
//
//  1. An explicit "<agent>: text" prefix (§9: "explicit override, always
//     wins") queues directly — this always bypasses auto-routing
//     entirely, at every routing mode, including the LLM-based one.
//  2. A bare "/name ..." matching a command any connected agent
//     advertised via available_commands_update routes straight to
//     whichever agent(s) declare it. Ambiguous (2+ agents declare the
//     same name) returns a pendingRoute restricted to just those agents.
//  3. Anything else — including a colon that isn't actually an agent
//     prefix, e.g. a colon appearing naturally inside free text — goes to
//     defaultAgent. (Keyword-based auto-routing was removed entirely —
//     chorus-spec.md §0 — in favor of either "off" (always defaultAgent,
//     this case) or an LLM-based routing decision; see the `context`
//     command / internal/router for the latter, layered on top of this
//     function from its caller, not inside it.)
//
// Returns the agent that should receive the prompt (empty if none should —
// on a usage/lookup error, or when ask/decisionReq is non-nil and the turn
// isn't resolved yet) and the exact text to send it (which for the
// "<agent>: text" form is the part after the colon, not the raw line) — the
// caller queues it (via Model.dispatchUserTurn) and echoes what was asked
// via render.FormatUserPrompt, since ACP has no mechanism for that to come
// back from the agent itself on a live turn. isPrompt distinguishes a
// free-text turn (true — eligible for a handoff preamble when it switches
// agents) from a slash command (false — a control command, never
// preamble-wrapped). Also returns any informational text to display (may be
// "") and, if a slash command was ambiguous, a non-nil pendingRoute the
// caller should prompt for and later resolve once the user answers.
//
// dispatch itself never queues or prints: bubbletea owns the terminal, and
// the handoff preamble needs Model state (lastRoutedAgent, the activity
// log) this free function doesn't have — so the single queue/echo/record
// choke point lives on the Model (dispatchUserTurn), and dispatch only
// resolves the destination.
func dispatch(line string, workers map[string]*AgentWorker, defaultAgent string, routing policy.Routing, commands map[string][]acp.AvailableCommand) (agent, sentText string, isPrompt bool, msg string, ask *pendingRoute, decisionReq *decisionRequest) {
	if a, rest, ok := strings.Cut(line, ":"); ok {
		a = strings.TrimSpace(a)
		rest = strings.TrimSpace(rest)
		// Only treat this as a prefix attempt if the part before ":" has
		// no spaces — "please fix this: it's broken" isn't someone typing
		// an agent name, so let it fall through to auto-routing instead.
		// This ALWAYS bypasses routing entirely, at every routing.Mode,
		// including "llm" — an explicit ask always wins.
		if a != "" && !strings.ContainsAny(a, " \t") {
			if rest == "" {
				return "", "", false, fmt.Sprintf("usage: <agent>: <text>  (agents: %s)", strings.Join(agentNames(workers), ", ")), nil, nil
			}
			if !knownAgent(a, workers) {
				return "", "", false, fmt.Sprintf("unknown agent %q (agents: %s)", a, strings.Join(agentNames(workers), ", ")), nil, nil
			}
			return a, rest, true, "", nil, nil
		}
	}

	if strings.HasPrefix(line, "/") {
		if owners := commandOwners(commands, line); len(owners) > 0 {
			if len(owners) == 1 {
				return owners[0], line, false, fmt.Sprintf("(/%s -> %s)", commandName(line), owners[0]), nil, nil
			}
			return "", "", false, "", &pendingRoute{text: line, candidates: owners}, nil
		}
		// No agent has advertised this command (yet, or at all) — fall
		// through to ordinary routing below rather than erroring; it
		// might just be prose that happens to start with "/".
	}

	if routing.ModeOrDefault() == policy.RoutingLLM {
		// Resolved asynchronously by the caller (Model.startRouteDecision)
		// — an LLM decision requires a real agent turn, which must never
		// block Model.Update (concurrency invariant #1).
		return "", "", false, "", nil, &decisionRequest{text: line}
	}

	if defaultAgent == "" || !knownAgent(defaultAgent, workers) {
		return "", "", false, fmt.Sprintf("no default agent configured or connected — use \"<agent>: text\" (agents: %s)", strings.Join(agentNames(workers), ", ")), nil, nil
	}
	return defaultAgent, line, true, "", nil, nil
}

// decisionRequest signals that dispatch couldn't resolve a prompt
// synchronously — routing.Mode is "llm" and no explicit prefix/slash
// command already handled it — and the caller must kick off an async LLM
// routing decision instead (see Model.startRouteDecision/runRouteDecision).
type decisionRequest struct {
	text string
}

// decisionAgentInfos builds router.DecisionAgentInfo for every CONNECTED
// agent (has a worker — some registry entries may have failed to start or
// get a session) in registry order, for BuildDecisionPrompt.
func decisionAgentInfos(specs []session.Spec, workers map[string]*AgentWorker) []router.DecisionAgentInfo {
	out := make([]router.DecisionAgentInfo, 0, len(specs))
	for _, s := range specs {
		if _, ok := workers[s.Name]; !ok {
			continue
		}
		out = append(out, router.DecisionAgentInfo{Name: s.Name, CostTier: s.CostTier, Notes: s.Notes, Models: s.Models})
	}
	return out
}

// runRouteDecision runs one LLM routing decision as a hidden sub-session
// on the decision agent's EXISTING connection — the identical shape
// internal/delegate.Hub.doDelegate already uses for delegation sub-
// sessions (fresh session, mcpServers nil, Collectors captures the reply
// so it never renders inline), reused here rather than inventing a
// second "hidden sub-session" mechanism. Runs as a tea.Cmd (bubbletea's
// own async pattern) since a real agent turn can never block
// Model.Update (concurrency invariant #1).
//
// timeout is caller-supplied (policy.Routing.DecisionTimeout(), configurable
// per project) rather than a fixed constant — found live (chorus-spec.md
// §0) that a free/shared-capacity decision agent's own upstream provider
// can be intermittently slow or need an internal retry, and a too-tight
// timeout here cuts that off, surfacing as a generic decision-failed
// error indistinguishable from a real problem: a stuck or tool-using
// decision call (the decision agent still has its own built-in tools
// available even with chorus's MCP servers detached; see
// BuildDecisionPrompt's explicit "do not use tools" instruction, which
// mitigates but can't structurally prevent this) must still never hang
// the router indefinitely — the timeout is just another failure path
// that falls back to the default agent, same as a parse failure; it just
// needs to be generous enough not to fire on ordinary slowness.
func runRouteDecision(ctx context.Context, conn *session.Connection, coll *delegate.Collectors, cwd string, agents []router.DecisionAgentInfo, defaultAgent, contextText, userPrompt string, reqID int, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		// Errors below are labeled by stage ("opening decision sub-
		// session" vs "decision sub-session prompt") since a bare ACP/
		// JSON-RPC error code (e.g. "-32603 Internal error") alone doesn't
		// say which call the target agent's ACP server actually failed —
		// found live to matter: check this project's own per-agent
		// .chorus/logs/<agent>.stderr.log first, since a generic -32603
		// almost always means the AGENT's own process hit an unhandled
		// error internally, not that chorus sent it something malformed.
		sub, err := conn.NewSession(cctx, cwd, nil)
		if err != nil {
			return routeDecisionMsg{reqID: reqID, prompt: userPrompt, err: fmt.Errorf("opening decision sub-session: %w", err)}
		}
		coll.Register(sub.SessionID)
		promptText := router.BuildDecisionPrompt(agents, defaultAgent, contextText, userPrompt)
		promptErr := sub.Prompt(cctx, promptText)
		reply := coll.Collect(sub.SessionID)
		if promptErr != nil {
			return routeDecisionMsg{reqID: reqID, prompt: userPrompt, err: fmt.Errorf("decision sub-session prompt: %w", promptErr)}
		}
		decision, err := router.ParseDecision(reply, agents)
		return routeDecisionMsg{reqID: reqID, prompt: userPrompt, decision: decision, err: err}
	}
}

// buildHandoffPreamble is prepended to the routed prompt when an LLM
// routing decision switches to a different agent than lastRoutedAgent —
// self-identification framing matches buildBriefingText/buildNudgeText
// (main.go/internal/acpclient), proven necessary live (chorus-spec.md
// §0: an agent otherwise suspects a plain instruction message is a
// prompt-injection attempt).
func buildHandoffPreamble(fromAgent, activity string) string {
	var b strings.Builder
	b.WriteString("This is automated handoff context from chorus itself, the multi-agent CLI harness you're " +
		"running under — not a message from the user. The user's task is continuing, but the previous turn(s) " +
		"were handled by a different agent (" + fromAgent + "), whose work you have no memory of. Here is what " +
		"happened so far, for context:\n\n")
	b.WriteString(activity)
	b.WriteString("\n\nThe user's actual next message follows.")
	return b.String()
}

// commandName extracts the command name from a "/name ..." line, without
// the leading slash.
func commandName(line string) string {
	name, _, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	return name
}

// commandOwners returns the sorted names of agents that have advertised a
// command matching line's leading "/name" (case-insensitive), based on
// the most recent available_commands_update each has sent.
func commandOwners(commands map[string][]acp.AvailableCommand, line string) []string {
	name := strings.ToLower(commandName(line))
	if name == "" {
		return nil
	}
	var owners []string
	for agent, cmds := range commands {
		for _, c := range cmds {
			if strings.ToLower(c.Name) == name {
				owners = append(owners, agent)
				break
			}
		}
	}
	sort.Strings(owners)
	return owners
}

// findCommandByAlias reports the first of agent's advertised commands
// whose name contains one of aliases, case-insensitively — used to
// discover a compaction-style command (Model.fireCompaction) or a
// model-switch command without ever hardcoding a literal name like
// "/compact" or "/model": different agents may name the same concept
// differently, or not support it at all, and chorus only finds out by
// asking (available_commands_update), never by guessing. Deliberately a
// substring match, more permissive than AutoAllowTool's prefix rule —
// this is read-only capability discovery, not a permission bypass, so
// there's no spoofing risk to guard against.
func findCommandByAlias(cmds []acp.AvailableCommand, aliases []string) (acp.AvailableCommand, bool) {
	for _, c := range cmds {
		lower := strings.ToLower(c.Name)
		for _, alias := range aliases {
			if alias != "" && strings.Contains(lower, strings.ToLower(alias)) {
				return c, true
			}
		}
	}
	return acp.AvailableCommand{}, false
}

// formatCommands lists every agent's currently known slash commands, for
// the "commands" REPL command — the discoverability chorus otherwise
// loses by wrapping each agent's native interface.
func formatCommands(commands map[string][]acp.AvailableCommand) string {
	agents := make([]string, 0, len(commands))
	for a := range commands {
		agents = append(agents, a)
	}
	sort.Strings(agents)

	var b strings.Builder
	if len(agents) == 0 {
		fmt.Fprintln(&b, "no agent has reported any commands yet")
		return b.String()
	}
	for _, a := range agents {
		fmt.Fprintf(&b, "%s:\n", a)
		cmds := commands[a]
		if len(cmds) == 0 {
			fmt.Fprintln(&b, "  (none)")
			continue
		}
		sorted := append([]acp.AvailableCommand(nil), cmds...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, c := range sorted {
			fmt.Fprintf(&b, "  /%-20s %s\n", render.StripANSI(c.Name), render.StripANSI(c.Description))
		}
	}
	return b.String()
}

// formatCapabilities prints each connected agent's advertised ACP
// capabilities that chorus's behavior actually branches on — session
// resume and image prompts. Intended for manually verifying a newly-added
// or newly-working agent against what's already known to work — see
// CLAUDE.md's Gemini verification playbook.
func formatCapabilities(specs []session.Spec, conns map[string]*session.Connection) string {
	var b strings.Builder
	for _, spec := range specs {
		conn, ok := conns[spec.Name]
		if !ok {
			fmt.Fprintf(&b, "%s: not connected\n", spec.Name)
			continue
		}
		fmt.Fprintf(&b, "%s:\n", spec.Name)
		fmt.Fprintf(&b, "  loadSession (resume):  %v\n", conn.SupportsLoadSession)
		fmt.Fprintf(&b, "  promptCapabilities.image: %v\n", conn.SupportsImagePrompts)
	}
	return b.String()
}

// formatStats prints each agent's direct-vs-delegated tool activity, plus
// its most recently reported token usage, for this run — the tool-call
// tallying is CLAUDE.md's delegation-maximization plan item 4, added so
// policy.yaml's delegation.prefer/nudge_threshold can be tuned against
// real numbers instead of guesswork; the token line (2026-09-04, user
// request — "show token consumption of agents in the session so far at a
// given point in time") reuses ACP's usage_update data trackUsage already
// captures for auto-compaction, previously discarded once that check ran.
// An agent shows up here if it has EITHER kind of activity — a
// conversational agent that's never called a tool but has burned real
// context still has something worth showing. specs is walked in registry
// order (the same determinism convention formatCapabilities/
// formatCommands use), not sorted, so this reads the same way run to run.
func formatStats(specs []session.Spec, s agentStats) string {
	var b strings.Builder
	anyActivity := false
	for _, spec := range specs {
		direct := s.direct[spec.Name]
		sent := s.delegateSent[spec.Name]
		recv := s.delegateRecv[spec.Name]
		failed := s.delegateFail[spec.Name]
		size, hasUsage := s.usageSize[spec.Name]
		used := s.usageUsed[spec.Name]
		if direct == 0 && sent == 0 && recv == 0 && !hasUsage {
			continue
		}
		anyActivity = true
		fmt.Fprintf(&b, "%s:\n", spec.Name)
		if hasUsage {
			if size > 0 {
				fmt.Fprintf(&b, "  tokens used:             %d/%d (%d%%)\n", used, size, used*100/size)
			} else {
				fmt.Fprintf(&b, "  tokens used:             %d\n", used)
			}
		}
		fmt.Fprintf(&b, "  direct tool calls:       %d\n", direct)
		if sent > 0 {
			fmt.Fprintf(&b, "  delegate calls sent:     %d", sent)
			if failed > 0 {
				fmt.Fprintf(&b, " (%d failed)", failed)
			}
			b.WriteString("\n")
		} else {
			fmt.Fprintf(&b, "  delegate calls sent:     0\n")
		}
		fmt.Fprintf(&b, "  delegate calls received: %d\n", recv)
	}
	if !anyActivity {
		return "no activity recorded yet this run\n"
	}
	return b.String()
}

// QueuePrompt is queuePrompt, exported for main.go's startup-phase use:
// the delegation briefing is queued before any Model exists (main.go's
// phase 3, alongside session creation), so it can't go through Model's
// own dispatch path. Returns an informational message on failure ("" on
// success).
func QueuePrompt(agent, text string, workers map[string]*AgentWorker) string {
	return queuePrompt(agent, text, workers)
}

// queuePrompt sends text (converted to content blocks) to agent's worker.
// Returns an informational message on failure ("" on success) instead of
// printing directly, since bubbletea owns the terminal.
func queuePrompt(agent, text string, workers map[string]*AgentWorker) string {
	w, ok := workers[agent]
	if !ok {
		return fmt.Sprintf("unknown agent %q", agent)
	}
	select {
	case w.in <- buildPromptBlocks(text):
		return ""
	default:
		return fmt.Sprintf("%s is still processing a backlog of prompts, try again shortly", agent)
	}
}

// buildPromptBlocks turns raw input text into the content-block sequence
// actually sent to the agent: any @path.png-style attachments (see
// extractAttachments) become leading acp.ImageBlocks, followed by a
// single TextBlock with the attachment tokens stripped out. If nothing
// but attachment tokens remain (or extraction found nothing at all), the
// original text is sent verbatim rather than an empty prompt.
func buildPromptBlocks(text string) []acp.ContentBlock {
	clean, images := extractAttachments(text)
	if len(images) == 0 {
		return []acp.ContentBlock{acp.TextBlock(text)}
	}
	blocks := make([]acp.ContentBlock, 0, len(images)+1)
	blocks = append(blocks, images...)
	if clean != "" {
		blocks = append(blocks, acp.TextBlock(clean))
	}
	return blocks
}

// attachmentRE matches an @-prefixed path ending in a common image
// extension, e.g. "@screenshot.png" or "@./docs/before.jpg". Deliberately
// simple (no quoting/escaping support) — this is an input-line
// convenience, not a shell.
var attachmentRE = regexp.MustCompile(`@(\S+\.(?:png|jpe?g|gif|webp))\b`)

// extractAttachments pulls @path.png-style tokens out of text, reading
// and base64-encoding each into an acp.ImageBlock. A token that doesn't
// resolve to a readable file is left in place (so the user sees it wasn't
// consumed) and a warning is printed to stderr, rather than silently
// dropping it or failing the whole prompt.
func extractAttachments(text string) (clean string, images []acp.ContentBlock) {
	clean = attachmentRE.ReplaceAllStringFunc(text, func(m string) string {
		path := m[1:] // strip leading "@"
		data, mime, err := readImageBase64(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: couldn't attach %s: %v\n", path, err)
			return m
		}
		images = append(images, acp.ImageBlock(data, mime))
		return ""
	})
	return strings.TrimSpace(clean), images
}

func readImageBase64(path string) (data, mime string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(b), mimeForImageExt(filepath.Ext(path)), nil
}

func mimeForImageExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// extractAgentText returns an update's agent_message_chunk text, if it
// has one. Only this update kind counts toward a delegation call's
// collected result — agent_thought_chunk and others are hidden from the
// interactive terminal when they belong to a sub-session but don't
// contribute reply text.
func extractAgentText(u bus.Update) (string, bool) {
	chunk := u.Notification.Update.AgentMessageChunk
	if chunk == nil || chunk.Content.Text == nil {
		return "", false
	}
	return chunk.Content.Text.Text, true
}

func formatDelegateLog(le delegate.LogEntry) string {
	task := le.Task
	if len(task) > 80 {
		task = task[:80] + "..."
	}
	status := "ok"
	if le.Err != nil {
		// StripANSI here even though this is normally a Go-level/protocol
		// error, not agent-authored text — see formatErrLine's comment
		// (internal/tui/model.go) for the same reasoning: a delegated
		// call's error can in principle echo back agent/subprocess-
		// controlled content, and sanitizing it costs nothing.
		status = "error: " + render.StripANSI(le.Err.Error())
	}
	return fmt.Sprintf("\n[delegate] %s -> %s: %q (%s, %s)\n", le.Source, le.Target, task, le.Duration.Round(time.Millisecond), status)
}

// formatRouteAskPrompt renders the question shown while a pendingRoute is
// awaiting an answer — appended as a block, not printed, so it stays
// visible in the scrollable document once answered.
// formatRouteAskPrompt renders the routeAsk agent-choice menu, with
// cursor marking the arrow-key-selected option (0-based) — the same
// visual convention as a permission prompt's menu (render.FormatMenuLine),
// so the two interactive menus in chorus look and behave consistently.
func formatRouteAskPrompt(ask *pendingRoute, workers map[string]*AgentWorker, cursor int) string {
	names := ask.candidates
	question := "no routing rule matched — which agent should handle this?"
	if names == nil {
		names = agentNames(workers)
	} else {
		question = fmt.Sprintf("more than one agent has a %q command — which did you mean?", "/"+commandName(ask.text))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n", question)
	for i, n := range names {
		fmt.Fprint(&b, render.FormatMenuLine(i, cursor, n))
	}
	return b.String()
}

// matchName resolves line against names by 1-based index or
// case-insensitive exact match.
func matchName(line string, names []string) (string, bool) {
	if idx, err := strconv.Atoi(line); err == nil {
		if idx >= 1 && idx <= len(names) {
			return names[idx-1], true
		}
		return "", false
	}
	lower := strings.ToLower(line)
	for _, n := range names {
		if strings.ToLower(n) == lower {
			return n, true
		}
	}
	return "", false
}

// answerPermission parses the user's reply to a permission prompt: either
// the 1-based option number or a case-insensitive match on the option's
// name/kind. Returns false (reprompt, don't consume) on an invalid reply.
func answerPermission(pending *bus.PermissionRequest, line string) bool {
	opts := pending.Req.Options
	lower := strings.ToLower(line)

	if idx, err := strconv.Atoi(line); err == nil {
		if idx >= 1 && idx <= len(opts) {
			selectOption(pending, opts[idx-1].OptionId)
			return true
		}
		return false
	}
	for _, o := range opts {
		if strings.ToLower(o.Name) == lower || strings.ToLower(string(o.Kind)) == lower {
			selectOption(pending, o.OptionId)
			return true
		}
	}
	if lower == "c" || lower == "cancel" {
		pending.Resp <- acp.RequestPermissionResponse{
			Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}},
		}
		return true
	}
	return false
}

func selectOption(pending *bus.PermissionRequest, id acp.PermissionOptionId) {
	pending.Resp <- acp.RequestPermissionResponse{
		Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: id}},
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func knownAgent(name string, workers map[string]*AgentWorker) bool {
	_, ok := workers[name]
	return ok
}

func agentNames(workers map[string]*AgentWorker) []string {
	names := make([]string, 0, len(workers))
	for n := range workers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
