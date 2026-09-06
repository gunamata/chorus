// Package render implements the §15 Renderer: turns raw ACP session/update
// notifications and permission requests into formatted text. Dispatched by
// a switch on the update's kind, with an unrecognized-kind fallback that
// always renders (tagged, not dropped) rather than silently discarding
// anything ACP might add later.
//
// Renderer is a pure formatting library: FormatUpdate/FormatSpinnerTick,
// given an update (or a spinner tick), return the text to show and a merge
// key, with no I/O of their own — safe to call from any render loop.
// internal/tui's Model is the current (and only) caller: it owns the
// terminal (via bubbletea) and lays out the returned text in a scrolling
// viewport. This package previously also owned an ANSI-cursor-redraw
// compatibility path (Render/TickSpinner, for chorus's original
// line-based REPL) — removed once internal/tui replaced that REPL, since
// nothing calls it anymore; see chorus-spec.md/CLAUDE.md for the history.
package render

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/glamour"
	acp "github.com/coder/acp-go-sdk"

	"chorus/internal/bus"
)

const (
	colReset  = "\x1b[0m"
	colDim    = "\x1b[2m"
	colBold   = "\x1b[1m"
	colRed    = "\x1b[31m"
	colGreen  = "\x1b[32m"
	colYellow = "\x1b[33m"
	colCyan   = "\x1b[36m"
	colMag    = "\x1b[35m"
	colBlue   = "\x1b[34m"
	// colHighlightBg sets a solid background — used only to set an echoed
	// user prompt visually apart from what follows it (FormatUserPrompt).
	// SGR 100 (bright black background) rather than a specific 256-color
	// code, since it works on any ANSI-compatible terminal without needing
	// extended color support.
	colHighlightBg = "\x1b[100m"
)

// spinnerFrames animates in-progress tool calls — see TickSpinner.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// SpinnerFrame returns the animation frame for tick (any monotonically
// increasing counter), using the same frame set FormatSpinnerTick animates
// in-document tool-call/thinking spinners with. Exported so internal/tui
// can animate its own persistent "agent busy" status line — which has no
// in-place block to attach to and so can't go through Renderer's own
// tick-driven FormatSpinnerTick/activeToolCall state — with a visually
// consistent spinner rather than a second, different-looking one.
func SpinnerFrame(tick int) rune {
	return spinnerFrames[tick%len(spinnerFrames)]
}

// thinkingWords are the whimsical single-word indicators shown while an
// agent is reasoning and ShowThoughts is off (the default) — a rotating
// word beats a wall of chain-of-thought text for a brief "still working"
// signal. One is picked per thinking burst (not re-rolled on every
// chunk — see formatThinkingIndicator) and animated by the same spinner
// tick as an in-progress tool call.
var thinkingWords = []string{
	"Pondering", "Percolating", "Noodling", "Ruminating", "Marinating",
	"Cogitating", "Mulling", "Puzzling", "Deliberating", "Contemplating",
	"Wrangling", "Untangling", "Synthesizing", "Simmering", "Musing",
}

// Renderer formats bus.Update values into displayable text.
type Renderer struct {
	ShowThoughts bool

	// toolCallTitles remembers each tool call's last known title, since
	// ToolCallUpdate.Title is optional — an update that omits it means
	// "unchanged", not "blank" (observed live: Claude sends follow-up
	// status updates with no title at all).
	toolCallTitles map[string]string

	// activeToolCall holds the arguments of the tool call currently
	// occupying the redraw region, if its status is in_progress — what
	// FormatSpinnerTick/TickSpinner re-renders on each animation frame.
	// Cleared whenever clearInPlaceState runs, so a stale/abandoned tool
	// call never keeps animating after something else has taken over.
	activeToolCall *toolCallState
	spinnerFrame   int

	// activeThought mirrors activeToolCall but for a hidden (ShowThoughts
	// false) thinking burst: the word is chosen once, when thinking
	// starts, and kept fixed for the burst's duration — only the spinner
	// frame animates. Cleared by dropStaleStreamBufs whenever a different
	// mergeable block takes over (a tool call starts, or the actual
	// message begins streaming), the same invalidation streamBufs gets.
	activeThought *thinkingState

	// streamBufs accumulates each live agent-message/thought stream's
	// full text so it can be re-rendered as markdown (which needs the
	// whole block, not per-chunk fragments) on every new chunk. Keyed by
	// "agent|kind" so a thought stream and a message stream for the same
	// agent don't collide. Cleared alongside the redraw state whenever
	// something else interrupts.
	streamBufs map[string]*strings.Builder

	// md renders markdown to ANSI-styled text sized to the terminal
	// width. nil (construction failed) falls back to printing text
	// unstyled — never a hard failure. width/styleOpt feed buildMarkdown;
	// see SetWidth/SetStyle.
	md       *glamour.TermRenderer
	width    int
	styleOpt glamour.TermRendererOption

	// imageDir, if set, is where inbound image content gets saved so it
	// can be referenced by path instead of collapsing to a bare "[image]"
	// placeholder — a terminal can't display the bytes inline, but a path
	// the user can open is far more useful than nothing. imageSeq numbers
	// them within one run.
	imageDir string
	imageSeq int
}

type toolCallState struct {
	agent   string
	id      acp.ToolCallId
	title   string
	status  acp.ToolCallStatus
	content []acp.ToolCallContent
}

type thinkingState struct {
	agent string
	word  string
}

// New constructs a Renderer at a default width (100) — call SetWidth once
// the real terminal size is known. In practice this is immediate: a
// bubbletea Program always sends an initial tea.WindowSizeMsg on startup,
// which internal/tui's Model forwards straight into SetWidth.
func New() *Renderer {
	r := &Renderer{
		// Off by default: full thinking text is verbose and most users
		// don't want a wall of chain-of-thought scrolling past. A brief
		// rotating-word indicator (formatThinkingIndicator) shows the
		// agent is working instead; toggle with the "thoughts" command
		// to see the full text.
		ShowThoughts:   false,
		toolCallTitles: make(map[string]string),
		streamBufs:     make(map[string]*strings.Builder),
		width:          100,
	}
	r.buildMarkdown()
	return r
}

// SetWidth rebuilds the markdown renderer for a new width — called by
// internal/tui's Model on every tea.WindowSizeMsg (initial size and every
// resize), since New() only sets a default width at construction.
func (r *Renderer) SetWidth(w int) {
	if w <= 0 {
		return
	}
	r.width = w
	r.buildMarkdown()
}

// SetStyle forces a specific glamour style instead of relying on
// glamour.WithAutoStyle()'s TTY auto-detection, which only behaves
// reliably when r.out is a real, directly-owned terminal fd — not the
// case once a TUI framework (which manages its own screen/renderer) owns
// the terminal. Also used by tests that need deterministic styled output
// regardless of the environment's TTY state. opt == nil restores
// auto-detection.
func (r *Renderer) SetStyle(opt glamour.TermRendererOption) {
	r.styleOpt = opt
	r.buildMarkdown()
}

func (r *Renderer) buildMarkdown() {
	opt := r.styleOpt
	if opt == nil {
		opt = glamour.WithAutoStyle()
	}
	if md, err := glamour.NewTermRenderer(opt, glamour.WithWordWrap(r.width)); err == nil {
		r.md = md
	}
	// If glamour construction failed, r.md stays nil (or keeps its
	// previous value) and renderMarkdown falls back to plain text — not
	// a fatal error for the whole tool.
}

// WithImageDir enables saving inbound images under dir (created if
// needed by the caller) instead of showing a bare "[image]" placeholder.
// Optional; without it, Render behaves exactly as before this feature.
func (r *Renderer) WithImageDir(dir string) *Renderer {
	r.imageDir = dir
	return r
}

// ansiRE matches terminal escape sequences: CSI (color, cursor movement —
// the kind chorus's own rendering uses deliberately), OSC (e.g. terminal
// title changes, some of which are exploitable in vulnerable terminal
// emulators), and simple two-byte ESC sequences.
var ansiRE = regexp.MustCompile("\x1b(?:\\[[0-9;?]*[a-zA-Z]|\\][^\a]*(?:\a|\x1b\\\\)|[@-Z\\\\-_])")

// StripANSI removes terminal escape sequences from s.
//
// chorus-spec.md §0's 2026-08-22 audit: every string in this file that
// originates from an agent (message text, diff content, plan entries,
// tool titles, permission option names, command names/descriptions) is
// printed via a bare fmt.Fprint* with chorus's own color codes wrapped
// around it. Nothing stopped agent-supplied text from embedding its own
// escape sequences — including ones a malicious or manipulated agent
// response (e.g. one influenced by a prompt-injected file it read) could
// use to hide/spoof terminal content or attempt to exploit a vulnerable
// terminal emulator. Applied at every point agent-controlled text is
// interpolated into output; never applied to chorus's own color
// constants, which are trusted literals, not agent input.
func StripANSI(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

func agentTag(agent string) string {
	color := colCyan
	switch agent {
	case "gemini":
		color = colMag
	case "opencode":
		color = colBlue
	}
	return fmt.Sprintf("%s[%s]%s", color, agent, colReset)
}

// FormatUpdate formats one update, returning the text to show and a merge
// key. key == "" means the block is a one-shot, always-appended line
// (never merged with whatever came before). A non-empty key means the
// block is mergeable: a caller building an ordered document should
// replace the previous block with this same key (if it was the most
// recently produced one) rather than appending a new one — this is
// exactly the "same key as before -> overwrite; different key -> finalize
// and start fresh" rule the old ANSI-redraw path (beginRedraw/endRedraw)
// implemented via cursor movement. ok == false means nothing should be
// shown at all (e.g. a filtered thought chunk, or a title-less
// session_info_update).
//
// This method mutates Renderer's per-agent/per-tool-call formatting
// state (toolCallTitles, activeToolCall, streamBufs, imageSeq — including
// saveImage's file-write side effect) exactly as Render always has; a
// caller must treat this as an Update()-time operation, never call it
// from a pure View()/redraw pass, or side effects like image saves would
// repeat on every frame.
func (r *Renderer) FormatUpdate(u bus.Update) (key, text string, ok bool) {
	up := u.Notification.Update
	switch {
	case up.AgentMessageChunk != nil:
		return r.formatStreamingText(u.Agent, "message", up.AgentMessageChunk.Content, "")

	case up.AgentThoughtChunk != nil:
		if !r.ShowThoughts {
			return r.formatThinkingIndicator(u.Agent, up.AgentThoughtChunk.Content)
		}
		return r.formatStreamingText(u.Agent, "thought", up.AgentThoughtChunk.Content, colDim+"(thinking)"+colReset)

	case up.UserMessageChunk != nil:
		r.clearInPlaceState()
		text := r.formatText(u.Agent, up.UserMessageChunk.Content, colDim)
		if text == "" {
			return "", "", false
		}
		return "", text, true

	case up.ToolCall != nil:
		tkey := u.Agent + "|" + string(up.ToolCall.ToolCallId)
		title := StripANSI(up.ToolCall.Title)
		r.toolCallTitles[tkey] = title
		k, text := r.formatToolCall(u.Agent, up.ToolCall.ToolCallId, title, up.ToolCall.Status, up.ToolCall.Content)
		return k, text, true

	case up.ToolCallUpdate != nil:
		tkey := u.Agent + "|" + string(up.ToolCallUpdate.ToolCallId)
		title := r.toolCallTitles[tkey]
		if up.ToolCallUpdate.Title != nil {
			title = StripANSI(*up.ToolCallUpdate.Title)
			r.toolCallTitles[tkey] = title
		}
		status := acp.ToolCallStatusPending
		if up.ToolCallUpdate.Status != nil {
			status = *up.ToolCallUpdate.Status
		}
		k, text := r.formatToolCall(u.Agent, up.ToolCallUpdate.ToolCallId, title, status, up.ToolCallUpdate.Content)
		return k, text, true

	case up.Plan != nil:
		r.clearInPlaceState()
		return "", r.formatPlan(u.Agent, up.Plan.Entries), true

	case up.AvailableCommandsUpdate != nil:
		// Deliberately doesn't list the commands inline — found live
		// (2026-09, user report): an agent with a large command set
		// (or one that re-reports this more than once per session, e.g.
		// after its first turn — concurrency invariant #8) turned every
		// occurrence into several lines of noise, most of it never
		// actually needed since `commands` already lists everything on
		// demand. A terse one-liner is enough to note "this agent is now
		// discoverable," without repeating the same list from scratch
		// every time it changes even slightly.
		r.clearInPlaceState()
		return "", fmt.Sprintf("%s %s(commands available — type \"commands\" to list)%s\n", agentTag(u.Agent), colDim, colReset), true

	case up.CurrentModeUpdate != nil:
		r.clearInPlaceState()
		return "", fmt.Sprintf("%s %s(mode: %s)%s\n", agentTag(u.Agent), colDim, StripANSI(string(up.CurrentModeUpdate.CurrentModeId)), colReset), true

	// usage_update and session_info_update are real, frequent, defined
	// update kinds (unlike the true unrecognized-kind case below) — but
	// low value to see on every turn, so they're logged quietly rather
	// than dumped as raw JSON. Analogous to §15's treatment of
	// available_commands_update/current_mode_update as "log, don't
	// prominently surface."
	case up.UsageUpdate != nil:
		r.clearInPlaceState()
		return "", fmt.Sprintf("%s %s(tokens: %d/%d)%s\n", agentTag(u.Agent), colDim, up.UsageUpdate.Used, up.UsageUpdate.Size, colReset), true

	case up.SessionInfoUpdate != nil && up.SessionInfoUpdate.Title != nil:
		r.clearInPlaceState()
		return "", fmt.Sprintf("%s %s(session: %s)%s\n", agentTag(u.Agent), colDim, StripANSI(*up.SessionInfoUpdate.Title), colReset), true

	case up.SessionInfoUpdate != nil:
		// No title in this update (e.g. just a timestamp bump) — nothing
		// worth showing.
		return "", "", false

	default:
		r.clearInPlaceState()
		return "", fmt.Sprintf("%s %s(unrecognized update kind) %s%s\n", agentTag(u.Agent), colYellow, rawJSON(up), colReset), true
	}
}

// FormatUserPrompt formats text the user is actively sending to agent, for
// internal/tui to echo into the document at the moment of submission.
// ACP has no mechanism for this to come back from the agent on a live
// turn — UserMessageChunk (see the FormatUpdate case below) only ever
// arrives during session/load history replay, not for a prompt this
// client itself just sent — so without an explicit echo, nothing records
// what was actually asked, only the reply that follows it. Found live: a
// real user, several turns into a session, had no way to tell which
// agent reply answered which question. Same visual convention as a
// resumed session's replayed user turns (agentTag + dim text), so a
// fresh prompt and a replayed-from-history one read identically.
func (r *Renderer) FormatUserPrompt(agent, text string) string {
	if text == "" {
		return ""
	}
	text = StripANSI(text)
	var b strings.Builder
	fmt.Fprintln(&b, agentTag(agent))
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintln(&b, r.HighlightLine(line))
	}
	return b.String()
}

// HighlightLine wraps line in a full-width background highlight — used
// to set an echoed user prompt visually apart from the reply that
// follows it — padded with colHighlightBg-colored spaces so the block
// reads as solid, not just colored text with a ragged right edge.
// Exported (2026-09) so
// internal/tui can reuse the exact same visual treatment for click-drag
// text selection (Model.applySelectionHighlight) — a selection highlight
// and a prompt echo are the same visual idea (mark these specific lines
// as distinct from surrounding output), just triggered differently.
//
// Padding is rune-counted (utf8.RuneCountInString), not measured as true
// terminal display width — a deliberate, documented approximation: a
// wide glyph (CJK, emoji) renders as 2 terminal columns but counts as 1
// rune here, which would under-pad by one column per such glyph. Real
// prompt text is overwhelmingly plain-width in practice, and getting
// this exactly right needs a terminal-width-aware library render.go
// doesn't otherwise depend on — not worth the addition for a cosmetic
// edge, but worth this comment if wide-character prompts ever become a
// real complaint.
func (r *Renderer) HighlightLine(line string) string {
	content := " " + line
	pad := r.width - utf8.RuneCountInString(content)
	if pad < 0 {
		pad = 0
	}
	return colHighlightBg + content + strings.Repeat(" ", pad) + colReset
}

// nativeCommandOutputLimit caps how much of a "!"-command's combined
// stdout/stderr gets rendered into the document — mirrors CreateTerminal's
// OutputByteLimit (internal/acpclient), same rationale: an unbounded
// command (a runaway build log, an accidental `find /`) shouldn't be able
// to balloon the in-memory document/viewport without limit.
const nativeCommandOutputLimit = 200_000

// toolCallContentPreviewLimit caps how much of a tool call's own text
// content (e.g. the full text of a file a "read" tool call returned)
// gets shown inline in the transcript. Found live (2026-09, user
// report): opencode's ACP adapter includes the complete file content in
// ToolCallContent for a read — without a cap, this dumped an entire file
// into the scrollback for a single tool call, and since formatToolCall
// re-runs on every spinner tick while the call is in_progress, the same
// full dump could appear more than once before the call completed. Much
// smaller than nativeCommandOutputLimit, which exists to preserve
// legitimately long output (a build/test log) in full — this is meant as
// a peek at what the agent saw, not a substitute for opening the file.
const toolCallContentPreviewLimit = 500

// truncateToolCallContent applies toolCallContentPreviewLimit, walking
// back to a rune boundary rather than a raw byte offset — mirrors
// FormatNativeCommandResult's identical concern: slicing mid-rune would
// produce invalid UTF-8 that renders corrupted.
func truncateToolCallContent(t string) string {
	if len(t) <= toolCallContentPreviewLimit {
		return t
	}
	cut := toolCallContentPreviewLimit
	for cut > 0 && !utf8.RuneStart(t[cut]) {
		cut--
	}
	return fmt.Sprintf("%s... (%d more chars)", t[:cut], len(t)-cut)
}

// nativeTag marks a "!"-command block as chorus's own native execution,
// not any agent's — a distinct color/glyph from agentTag so it reads
// unambiguously as "chorus ran this directly, no LLM involved" even at a
// glance, matching CLAUDE.md's plan-item-#1 rationale: deterministic work
// (a build, a test run, a lint check) doesn't need an agent turn, metered
// or free, to execute.
func nativeTag() string {
	return colYellow + "[!]" + colReset
}

// FormatNativeCommandEcho formats a "!"-command the moment it's dispatched
// — the same "echo what was actually asked" convention FormatUserPrompt
// uses for agent prompts (chorus-spec.md/CLAUDE.md: found live that a
// submitted prompt needs to be echoed since nothing else records it), so a
// native command reads the same way in the scrollback: the exact command
// line on a highlighted background, immediately, before its result
// streams in.
func (r *Renderer) FormatNativeCommandEcho(cmdline string) string {
	if cmdline == "" {
		return ""
	}
	cmdline = StripANSI(cmdline)
	var b strings.Builder
	fmt.Fprintln(&b, nativeTag())
	fmt.Fprintln(&b, r.HighlightLine(cmdline))
	return b.String()
}

// FormatNativeCommandResult formats a completed "!"-command's output.
// output is StripANSI'd and length-capped the same way agent-supplied
// text is (security invariant #3, CLAUDE.md) — even though this text
// originates from a command the user themselves chose to run, not from an
// agent, it's still external, untrusted-by-construction text that could
// otherwise corrupt the viewport's rendering the same way agent output
// could; cheap to sanitize defensively regardless of source. runErr is
// whatever exec.Cmd.CombinedOutput's error was (nil on exit 0, an
// *exec.ExitError on a nonzero exit, or a context/start error) — reported
// as-is rather than special-cased, since Go's own "exit status N" text is
// already clear.
func FormatNativeCommandResult(cmdline, output string, runErr error, dur time.Duration) string {
	output = strings.TrimRight(StripANSI(output), "\n")
	truncated := false
	if len(output) > nativeCommandOutputLimit {
		// Walk back to a rune boundary rather than slicing at a raw byte
		// offset — otherwise a multi-byte UTF-8 character straddling the
		// cut point gets sliced mid-rune, producing invalid UTF-8 that
		// renders corrupted (mirrors terminalWriter.Write's same
		// RuneStart-walking fix in internal/acpclient/client.go).
		cut := nativeCommandOutputLimit
		for cut > 0 && !utf8.RuneStart(output[cut]) {
			cut--
		}
		output = output[:cut]
		truncated = true
	}

	var b strings.Builder
	if output != "" {
		fmt.Fprintln(&b, output)
	}
	if truncated {
		fmt.Fprintf(&b, "%s(output truncated at %d bytes)%s\n", colDim, nativeCommandOutputLimit, colReset)
	}
	if runErr != nil {
		fmt.Fprintf(&b, "%s%s failed after %s: %s%s\n", colRed, nativeTag(), dur.Round(time.Millisecond), StripANSI(runErr.Error()), colReset)
	} else {
		fmt.Fprintf(&b, "%s%s done in %s%s\n", colDim, nativeTag(), dur.Round(time.Millisecond), colReset)
	}
	fmt.Fprintln(&b)
	return b.String()
}

func (r *Renderer) formatText(agent string, cb acp.ContentBlock, prefix string) string {
	text := r.contentText(cb)
	if text == "" {
		return ""
	}
	if prefix != "" {
		return fmt.Sprintf("%s %s%s%s\n", agentTag(agent), prefix, text, colReset)
	}
	return fmt.Sprintf("%s %s\n", agentTag(agent), text)
}

// formatStreamingText accumulates cb's text into the named stream's
// buffer and re-renders the WHOLE buffer as markdown on every chunk.
// Whole-buffer re-rendering (not per-chunk) is deliberate: markdown needs
// to see a complete construct (a closing "**", a full code fence) to
// style it correctly, so re-parsing the growing buffer each time — rather
// than styling each raw chunk independently — is what keeps bold text and
// code blocks from rendering broken mid-stream.
func (r *Renderer) formatStreamingText(agent, kind string, cb acp.ContentBlock, label string) (key, text string, ok bool) {
	t := r.contentText(cb)
	if t == "" {
		return "", "", false
	}

	key = "stream|" + agent + "|" + kind
	r.dropStaleStreamBufs(key)

	buf, exists := r.streamBufs[key]
	if !exists {
		buf = &strings.Builder{}
		r.streamBufs[key] = buf
	}
	buf.WriteString(t)

	rendered := r.renderMarkdown(buf.String())
	header := agentTag(agent)
	if label != "" {
		header += " " + label
	}

	var b strings.Builder
	fmt.Fprintln(&b, header)
	if rendered != "" {
		fmt.Fprintln(&b, rendered)
	}
	return key, b.String(), true
}

// formatThinkingIndicator is the ShowThoughts-off counterpart to
// formatStreamingText's "thought" case: rather than the full reasoning
// text, it shows a brief "<agent> <spinner> <word>…" line — the word
// picked once per thinking burst (not re-rolled on every chunk, so it
// doesn't flicker) and animated by the same spinner tick as an
// in-progress tool call (FormatSpinnerTick). cb is only consulted to
// confirm this chunk actually carries text — an empty/unrenderable one
// shouldn't (re)start a thinking burst.
func (r *Renderer) formatThinkingIndicator(agent string, cb acp.ContentBlock) (key, text string, ok bool) {
	if r.contentText(cb) == "" {
		return "", "", false
	}

	key = "stream|" + agent + "|thought"
	r.dropStaleStreamBufs(key)

	if r.activeThought == nil || r.activeThought.agent != agent {
		r.activeThought = &thinkingState{agent: agent, word: thinkingWords[rand.Intn(len(thinkingWords))]}
	}
	return key, r.renderThinkingIndicator(), true
}

func (r *Renderer) renderThinkingIndicator() string {
	t := r.activeThought
	frame := spinnerFrames[r.spinnerFrame%len(spinnerFrames)]
	return fmt.Sprintf("%s %s%c %s…%s\n", agentTag(t.agent), colYellow, frame, t.word, colReset)
}

// renderMarkdown renders text as ANSI-styled markdown, or returns it
// unchanged if glamour wasn't available (construction failed) or on a
// per-call render error — a formatting problem should never be able to
// make output disappear.
func (r *Renderer) renderMarkdown(text string) string {
	if r.md == nil || strings.TrimSpace(text) == "" {
		return text
	}
	out, err := r.md.Render(text)
	if err != nil {
		return text
	}
	return strings.TrimRight(out, "\n")
}

func (r *Renderer) contentText(cb acp.ContentBlock) string {
	switch {
	case cb.Text != nil:
		return StripANSI(cb.Text.Text)
	case cb.Image != nil:
		return r.saveImage(cb.Image)
	case cb.Audio != nil:
		return "[audio]"
	case cb.ResourceLink != nil:
		return fmt.Sprintf("[resource: %s]", cb.ResourceLink.Uri)
	case cb.Resource != nil:
		return "[embedded resource]"
	default:
		return ""
	}
}

// saveImage writes img's decoded bytes under imageDir and returns a
// reference to where it landed, or a bare "[image]" placeholder if no
// imageDir is configured (WithImageDir was never called) — the original
// v1 behavior, kept as the fallback rather than the only option.
//
// This is the one place in this file with a side effect beyond Renderer's
// own formatting state: it writes a file. It's only ever reachable
// through FormatUpdate (via contentText), which callers must treat as an
// Update()-time operation — never call FormatUpdate from a pure View(), or
// this would re-save the same image on every frame redraw.
func (r *Renderer) saveImage(img *acp.ContentBlockImage) string {
	if r.imageDir == "" || img.Data == "" {
		return "[image]"
	}
	data, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil {
		return "[image: failed to decode]"
	}
	r.imageSeq++
	name := fmt.Sprintf("image-%03d%s", r.imageSeq, extForImageMime(img.MimeType))
	path := filepath.Join(r.imageDir, name)
	// Owner-only (0o600): this may be a screenshot or other content the
	// agent considered worth showing you — no reason for it to be
	// world-readable on a multi-user machine.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Sprintf("[image: failed to save: %v]", err)
	}
	return fmt.Sprintf("[image saved: %s]", path)
}

func extForImageMime(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".bin"
	}
}

// formatToolCall formats a tool call's status line, plus any diff content
// it carries, and returns it keyed for merging (§15: "update the same
// line in place as status changes arrive rather than printing a new line
// per transition, or the output turns to spam"). Also called by
// FormatSpinnerTick to reformat just the animation frame of an
// in_progress call — see there for why re-invoking this (rather than a
// separate incremental path) is the simplest correct way to do that.
func (r *Renderer) formatToolCall(agent string, id acp.ToolCallId, title string, status acp.ToolCallStatus, content []acp.ToolCallContent) (key, text string) {
	key = "tool|" + agent + "|" + string(id)
	r.dropStaleStreamBufs(key)

	var b strings.Builder
	icon := r.statusIcon(status)
	fmt.Fprintf(&b, "%s %s %s (%s)\n", agentTag(agent), icon, title, status)

	for _, c := range content {
		switch {
		case c.Diff != nil:
			fmt.Fprint(&b, renderDiff(*c.Diff))
		case c.Content != nil:
			t := r.contentText(c.Content.Content)
			if t != "" {
				fmt.Fprintf(&b, "%s  %s%s\n", colDim, truncateToolCallContent(t), colReset)
			}
		case c.Terminal != nil:
			fmt.Fprintf(&b, "%s  [terminal output omitted]%s\n", colDim, colReset)
		}
	}

	if status == acp.ToolCallStatusInProgress {
		r.activeToolCall = &toolCallState{agent: agent, id: id, title: title, status: status, content: content}
	} else if r.activeToolCall != nil && r.activeToolCall.agent == agent && r.activeToolCall.id == id {
		r.activeToolCall = nil
	}

	return key, b.String()
}

// FormatSpinnerTick advances the animation frame for whichever tool call
// is currently in_progress, if any, and returns its reformatted block.
// ok == false means nothing is in progress — a caller must not touch its
// display in that case, since ticking on a timer while idle must never
// disturb whatever else is currently shown (e.g. mid-typed input).
func (r *Renderer) FormatSpinnerTick() (key, text string, ok bool) {
	if r.activeToolCall != nil {
		r.spinnerFrame++
		t := r.activeToolCall
		key, text = r.formatToolCall(t.agent, t.id, t.title, t.status, t.content)
		return key, text, true
	}
	if r.activeThought != nil {
		r.spinnerFrame++
		key = "stream|" + r.activeThought.agent + "|thought"
		return key, r.renderThinkingIndicator(), true
	}
	return "", "", false
}

func (r *Renderer) statusIcon(s acp.ToolCallStatus) string {
	switch s {
	case acp.ToolCallStatusPending:
		return colDim + "○" + colReset
	case acp.ToolCallStatusInProgress:
		frame := spinnerFrames[r.spinnerFrame%len(spinnerFrames)]
		return colYellow + string(frame) + colReset
	case acp.ToolCallStatusCompleted:
		return colGreen + "●" + colReset
	case acp.ToolCallStatusFailed:
		return colRed + "✗" + colReset
	default:
		return "?"
	}
}

// dropStaleStreamBufs drops every streaming-text buffer except key's.
//
// Found via a failing test, not by inspection: a tool call interrupting a
// message stream left that stream's buffer sitting in streamBufs
// untouched, since the tool call's own key never matched it — so the next
// chunk of a same-kind stream later silently glued new text onto old,
// already-finalized content instead of starting fresh. Called whenever a
// new mergeable block (stream or tool call) is about to be formatted:
// switching to any different redraw target means whatever was streaming
// before is done.
func (r *Renderer) dropStaleStreamBufs(key string) {
	for k := range r.streamBufs {
		if k != key {
			delete(r.streamBufs, k)
		}
	}
	if r.activeThought != nil && "stream|"+r.activeThought.agent+"|thought" != key {
		r.activeThought = nil
	}
}

// clearInPlaceState resets the state that tracks an in-progress
// mergeable block — called whenever a new update is NOT a continuation
// of one (a fresh message, a tool call finishing, etc.) so a stale
// spinner or stream buffer never lingers past what it belongs to.
func (r *Renderer) clearInPlaceState() {
	r.activeToolCall = nil
	r.activeThought = nil
	for k := range r.streamBufs {
		delete(r.streamBufs, k)
	}
}

// formatPlan renders the agent's plan as a checklist. ACP replaces the
// entire plan on each update, so this always renders the full list.
func (r *Renderer) formatPlan(agent string, entries []acp.PlanEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %splan:%s\n", agentTag(agent), colBold, colReset)
	for _, e := range entries {
		box := "[ ]"
		switch e.Status {
		case acp.PlanEntryStatusInProgress:
			box = colYellow + "[~]" + colReset
		case acp.PlanEntryStatusCompleted:
			box = colGreen + "[x]" + colReset
		}
		fmt.Fprintf(&b, "  %s %s\n", box, StripANSI(e.Content))
	}
	return b.String()
}

// FormatPermissionPrompt renders a permission request's title and
// whatever option set it actually carries (§15: never a hardcoded menu).
// FormatPermissionPrompt renders a permission request as a menu, with
// cursor marking the arrow-key-selected option (0-based). Pure — call
// repeatedly as the cursor moves; internal/tui's Model tracks the block
// this landed in and re-renders it in place on each arrow keypress. It
// does NOT call ClearInPlaceState itself (unlike before arrow-key
// selection existed) — a menu can stay pending for a while with other
// agents' unrelated output streaming in around it, so clearing
// in-place state (which affects THEIR spinners/streams) must happen
// exactly once, when the prompt first arrives, not on every re-render;
// callers do that explicitly via ClearInPlaceState.
func (r *Renderer) FormatPermissionPrompt(agent string, req acp.RequestPermissionRequest, cursor int) string {
	title := "(no title)"
	if req.ToolCall.Title != nil {
		title = StripANSI(*req.ToolCall.Title)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s %sPERMISSION%s: %s wants to: %s\n", agentTag(agent), colYellow+colBold, colReset, agent, title)
	for i, o := range req.Options {
		fmt.Fprint(&b, FormatMenuLine(i, cursor, fmt.Sprintf("%s %s(%s)%s", StripANSI(o.Name), colDim, o.Kind, colReset)))
	}
	fmt.Fprint(&b, colDim+"↑/↓ + enter, or type a number/name/\"cancel\""+colReset+"\n")
	return b.String()
}

// ClearInPlaceState resets in-progress spinner/stream tracking — exported
// for internal/tui's Model to call exactly once when a NEW interactive
// menu (permission or routeAsk) first appears, since FormatPermissionPrompt
// itself no longer does this on every re-render (see its doc comment).
func (r *Renderer) ClearInPlaceState() {
	r.clearInPlaceState()
}

// FormatMenuLine renders one arrow-key-selectable menu option: a
// highlighted "❯ N) ..." for the cursor row, "  N) ..." otherwise.
// Exported so internal/tui's routeAsk menu (formatRouteAskPrompt) uses
// the identical visual convention as a permission prompt's menu, rather
// than a second, subtly-different implementation.
func FormatMenuLine(i, cursor int, rest string) string {
	if i == cursor {
		return fmt.Sprintf("%s%s❯ %d) %s%s\n", colBold, colGreen, i+1, rest, colReset)
	}
	return fmt.Sprintf("  %d) %s\n", i+1, rest)
}

// --- diff rendering -------------------------------------------------

// renderDiff formats an old/new text pair as a colored unified diff.
func renderDiff(d acp.ToolCallContentDiff) string {
	old := ""
	if d.OldText != nil {
		old = StripANSI(*d.OldText)
	}
	path := StripANSI(d.Path)
	oldLines := splitLines(old)
	newLines := splitLines(StripANSI(d.NewText))
	ops := lcsDiffOps(oldLines, newLines)
	hunks := groupHunks(ops, 3)

	var b strings.Builder
	fmt.Fprintf(&b, "%s  --- %s%s\n", colDim, path, colReset)
	fmt.Fprintf(&b, "%s  +++ %s%s\n", colDim, path, colReset)
	for _, h := range hunks {
		fmt.Fprintf(&b, "%s  @@ -%d,%d +%d,%d @@%s\n", colCyan, h.oldStart+1, h.oldCount, h.newStart+1, h.newCount, colReset)
		for _, op := range h.ops {
			switch op.kind {
			case opEq:
				fmt.Fprintf(&b, "%s   %s%s\n", colDim, op.text, colReset)
			case opDel:
				fmt.Fprintf(&b, "%s  -%s%s\n", colRed, op.text, colReset)
			case opAdd:
				fmt.Fprintf(&b, "%s  +%s%s\n", colGreen, op.text, colReset)
			}
		}
	}
	return b.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

type opKind int

const (
	opEq opKind = iota
	opDel
	opAdd
)

type diffOp struct {
	kind   opKind
	text   string
	oldIdx int // index into oldLines, valid for opEq/opDel
	newIdx int // index into newLines, valid for opEq/opAdd
}

// lcsDiffOps computes a line-level diff via a longest-common-subsequence
// DP table. O(n*m); fine for interactive single-file diffs.
func lcsDiffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{kind: opEq, text: a[i], oldIdx: i, newIdx: j})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{kind: opDel, text: a[i], oldIdx: i})
			i++
		default:
			ops = append(ops, diffOp{kind: opAdd, text: b[j], newIdx: j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{kind: opDel, text: a[i], oldIdx: i})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{kind: opAdd, text: b[j], newIdx: j})
	}
	return ops
}

type hunk struct {
	oldStart, oldCount int
	newStart, newCount int
	ops                []diffOp
}

// groupHunks splits a flat op list into unified-diff-style hunks, keeping
// `context` lines of unchanged text around each change and collapsing long
// unchanged runs between changes instead of printing whole files.
func groupHunks(ops []diffOp, context int) []hunk {
	if len(ops) == 0 {
		return nil
	}

	changeIdx := make([]int, 0)
	for idx, op := range ops {
		if op.kind != opEq {
			changeIdx = append(changeIdx, idx)
		}
	}
	if len(changeIdx) == 0 {
		return nil
	}

	// Determine [start,end) ranges of ops to include per hunk, merging
	// ranges that touch after adding context.
	type rng struct{ start, end int }
	var ranges []rng
	for _, ci := range changeIdx {
		start := max(0, ci-context)
		end := min(len(ops), ci+context+1)
		if len(ranges) > 0 && start <= ranges[len(ranges)-1].end {
			ranges[len(ranges)-1].end = end
		} else {
			ranges = append(ranges, rng{start, end})
		}
	}

	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })

	var hunks []hunk
	for _, rg := range ranges {
		h := hunk{ops: ops[rg.start:rg.end]}
		firstOld, firstNew := -1, -1
		for _, op := range h.ops {
			switch op.kind {
			case opEq:
				if firstOld == -1 {
					firstOld, firstNew = op.oldIdx, op.newIdx
				}
				h.oldCount++
				h.newCount++
			case opDel:
				if firstOld == -1 {
					firstOld = op.oldIdx
				}
				h.oldCount++
			case opAdd:
				if firstNew == -1 {
					firstNew = op.newIdx
				}
				h.newCount++
			}
		}
		if firstOld == -1 {
			firstOld = 0
		}
		if firstNew == -1 {
			firstNew = 0
		}
		h.oldStart, h.newStart = firstOld, firstNew
		hunks = append(hunks, h)
	}
	return hunks
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func rawJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%+v", v)
	}
	return string(b)
}
