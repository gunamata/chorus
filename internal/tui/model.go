// Package tui implements chorus's bubbletea-based interactive terminal UI:
// a scrolling output viewport plus a fixed input box, replacing the
// original line-based REPL (ANSI cursor-redraw + bufio.Scanner) that lived
// in main.go. See the implementation plan (chorus-spec.md/CLAUDE.md, and
// the design doc this was built from) for the full architecture rationale.
//
// Model owns the channel bridging (outputCh/permCh/errCh/doneCh/
// delegateLogCh), the permission/routeAsk state machine, and the
// viewport/textarea wiring. internal/render's Renderer stays the pure "how
// do I format one bus.Update" layer — Model is the new "how do I lay that
// out on screen and drive it from bubbletea's event loop" layer.
package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"

	acp "github.com/coder/acp-go-sdk"

	"chorus/internal/bus"
	"chorus/internal/delegate"
	"chorus/internal/policy"
	"chorus/internal/render"
	"chorus/internal/router"
	"chorus/internal/session"
)

// spinnerInterval matches the original REPL's TickSpinner cadence
// (main.go's old spinnerTicker) — slow enough to be cheap, fast enough to
// read as "working" rather than "frozen."
const spinnerInterval = 150 * time.Millisecond

// inputMode is a derived (not stored) view of which of pendingPerm/
// pendingRoute is active, if either — used for display and to route
// Enter-key handling. Kept as two independent optional Model fields
// rather than one flat enum: a permission request can interrupt and
// display over an outstanding routeAsk WITHOUT discarding it — the
// routeAsk resumes once the permission is answered. Collapsing to one
// enum would silently lose that (this exact nuance has no prior unit
// coverage — see model_test.go).
type inputMode int

const (
	modeNormal inputMode = iota
	modeRouteAsk
	modePermission
	// modeContextLevel is the `context` REPL command's live arrow-key menu
	// for routing.ContextLevel (chorus-spec.md §0's 2026-08-25 entry) —
	// lowest priority of the three menu modes: a permission or routeAsk
	// interrupting it is fine, this is a settings toggle, not a blocking
	// question about in-flight work.
	modeContextLevel
)

// agentStats tallies one agent's direct-vs-delegated tool activity —
// CLAUDE.md's delegation-maximization plan item 4, added so
// policy.yaml's delegation.prefer/nudge_threshold can eventually be
// tuned against real numbers instead of guesswork (see
// policy.defaultNudgeThreshold's own doc comment, which names this
// command directly). Built entirely from data already flowing through
// Model — ToolCall notifications on outputCh, delegate.LogEntry on
// delegateLogCh — so no new plumbing from internal/acpclient or
// internal/delegate was needed to add it.
type agentStats struct {
	direct       map[string]int // agent -> non-delegate ToolCall count
	delegateSent map[string]int // source agent -> delegate calls made
	delegateRecv map[string]int // target agent -> delegate calls received
	delegateFail map[string]int // source agent -> delegate calls that errored
}

func newAgentStats() agentStats {
	return agentStats{
		direct:       make(map[string]int),
		delegateSent: make(map[string]int),
		delegateRecv: make(map[string]int),
		delegateFail: make(map[string]int),
	}
}

func (m Model) mode() inputMode {
	if m.pendingPerm != nil {
		return modePermission
	}
	if m.pendingRoute != nil {
		return modeRouteAsk
	}
	if m.contextMenuOpen {
		return modeContextLevel
	}
	return modeNormal
}

// block is one entry in the ordered document Model renders into the
// viewport — the slice-based replacement for the old REPL's ANSI
// cursor-redraw (beginRedraw/endRedraw). A non-empty key means this block
// is mergeable: mergeBlock overwrites the block in place if it shares a
// key with (and immediately follows) the current last block, exactly the
// "same key as before -> overwrite; different key -> finalize and start
// fresh" rule the ANSI path implemented via cursor movement.
type block struct {
	key  string
	text string
}

// activityEntry is one merged-by-turn record in Model.activity — see its
// doc comment. key is "role:agent" (e.g. "user:claude" or "agent:claude"),
// used to merge consecutive same-turn chunks together (appendActivity)
// instead of one entry per streamed chunk.
type activityEntry struct {
	key  string
	text string
}

// Config bundles everything Model needs at construction — see main.go's
// phase 3, which builds all of this before ever constructing a Model
// (agent connect, delegate hub, session create/resume all stay exactly as
// they were; only what used to be the select-loop REPL is replaced).
type Config struct {
	Ctx context.Context

	Renderer   *render.Renderer
	Collectors *delegate.Collectors

	Workers      map[string]*AgentWorker
	DefaultAgent string
	Routing      policy.Routing
	Compaction   policy.Compaction
	Cwd          string
	AgentSpecs   []session.Spec
	Conns        map[string]*session.Connection

	OutputCh      chan bus.Update
	PermCh        chan bus.PermissionRequest
	ErrCh         chan ErrMsg
	DoneCh        chan PromptDoneMsg
	DelegateLogCh chan delegate.LogEntry
}

// Model is chorus's bubbletea Model — see the package doc.
type Model struct {
	ctx context.Context

	renderer *render.Renderer
	coll     *delegate.Collectors

	workers      map[string]*AgentWorker
	defaultAgent string
	routing      policy.Routing
	cwd          string
	agentSpecs   []session.Spec
	conns        map[string]*session.Connection
	commands     map[string][]acp.AvailableCommand
	stats        agentStats

	// activity is a bounded, merged-by-turn log of what's happened in the
	// interactive session — the mechanical (no extra LLM call) backing for
	// all three routing.ContextLevel tiers (chorus-spec.md §0's 2026-08-25
	// entry): "prompt" skips it, "digest" takes the last few entries,
	// "full" takes the whole (still capped) thing. Also what a cross-agent
	// handoff preamble is built from. lastRoutedAgent is which agent most
	// recently received a prompt, by ANY dispatch path (explicit prefix,
	// slash command, or LLM routing decision) — used to detect an actual
	// agent switch worth a handoff, regardless of how the switch happened.
	activity        []activityEntry
	lastRoutedAgent string
	// nextDecisionID tags each async routing-decision call so its
	// eventual routeDecisionMsg is traceable back to the request that
	// spawned it (diagnostic — correctness doesn't depend on it, since
	// nothing needs to cancel or de-duplicate in-flight decisions today).
	nextDecisionID int

	// contextMenuOpen/contextCursor/contextMenuIndex back the `context`
	// REPL command's live arrow-key menu for routing.ContextLevel —
	// mirrors permCursor/permMenuIndex's pattern exactly (see that field's
	// doc comment for why a menu tracks its own block position instead of
	// relying on mergeBlock).
	contextMenuOpen  bool
	contextCursor    int
	contextMenuIndex int

	// compaction, compactTriggered, compactPending back the auto-compact
	// feature (chorus-spec.md §0's 2026-08-25 entry): compactTriggered
	// tracks which agents are already at-or-past threshold (so the same
	// crossing doesn't re-fire on every subsequent usage update — cleared
	// once usage drops back below threshold, e.g. after a successful
	// compaction); compactPending tracks an agent that crossed threshold
	// while mid-turn, fired once its worker goes idle (checked from the
	// promptDoneMsg case) rather than interrupting it.
	compaction       policy.Compaction
	compactTriggered map[string]bool
	compactPending   map[string]bool

	outputCh      chan bus.Update
	permCh        chan bus.PermissionRequest
	errCh         chan ErrMsg
	doneCh        chan PromptDoneMsg
	delegateLogCh chan delegate.LogEntry

	viewport viewport.Model
	input    textarea.Model

	// promptHistory records every non-empty line submitted via
	// handleNormalLine (agent prompts, native "!" commands, meta commands
	// like "stats") in submission order, so Up at the top of the (now
	// multi-line) input box can recall it — see historyUp/historyDown.
	// historyIndex is -1 when not currently navigating history (the input
	// holds the user's own in-progress draft); historyDraft holds that
	// draft while navigating, so paging back down past the newest history
	// entry restores it instead of leaving the input blank.
	promptHistory []string
	historyIndex  int
	historyDraft  string

	// uiTick counts spinnerTickMsg deliveries unconditionally (unlike
	// renderer.spinnerFrame, which only advances while an in-place
	// document block is actively animating) — used purely to animate the
	// persistent "<agent> working (...)" status line in View(), which has
	// no document block of its own to attach a frame counter to.
	uiTick int

	blocks []block

	pendingPerm  *bus.PermissionRequest
	pendingRoute *pendingRoute

	// permCursor/routeCursor are the arrow-key-selected index (0-based)
	// into the currently pending menu's options, independently tracked
	// per menu type — not one shared field — specifically so the
	// permission-interrupts-routeAsk nuance (mode.go's doc comment)
	// doesn't leave a resumed routeAsk showing a cursor position left
	// over from an unrelated permission menu the user was navigating in
	// between. permMenuIndex/routeMenuIndex are each menu's position in
	// m.blocks, so an arrow keypress can update that exact block in
	// place (bypassing mergeBlock's "only the immediately preceding
	// block" rule — correct for short-lived tool-call/stream blocks,
	// wrong here: a menu can stay pending while OTHER agents' unrelated
	// output keeps getting appended after it). -1 means no menu of that
	// type is currently tracked.
	permCursor     int
	permMenuIndex  int
	routeCursor    int
	routeMenuIndex int

	width, height int
	ready         bool // true once the first WindowSizeMsg has sized viewport/input

	quitting bool
}

// inputHeight is the textarea's fixed visible height in rows. Not
// dynamically grown with content — bubbles/textarea is itself backed by an
// internal scrollable viewport (see its SetHeight doc comment), so a long
// or many-line prompt simply scrolls within this fixed box exactly like a
// small editor window, the same way the old single-line textinput never
// needed to grow either.
const inputHeight = 3

// inputKeyMap is textarea.DefaultKeyMap with InsertNewline rebound to
// ctrl+j only (dropping "enter"/"ctrl+m") — Enter must submit the prompt,
// handled directly in handleKey before any KeyMsg reaches the textarea, so
// insert-newline needs its own dedicated key instead. Everything else
// (Home/End -> line start/end, ctrl+a/ctrl+e as the same, which happens to
// already be exactly what macOS Terminal/iTerm's readline-style bindings
// expect) is DefaultKeyMap unchanged.
var inputKeyMap = func() textarea.KeyMap {
	km := textarea.DefaultKeyMap
	km.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"), key.WithHelp("ctrl+j", "insert newline"))
	return km
}()

// New constructs the initial Model. Call tea.NewProgram(New(cfg), ...) —
// see main.go.
func New(cfg Config) Model {
	ta := textarea.New()
	// Prompt was never set before textinput's own default ("" — no visual
	// marker at all for "this is where you type," unlike Claude Code/
	// Gemini CLI's own input lines). "❯ " matches the same cursor glyph the
	// arrow-key menu selection already uses (render.FormatMenuLine), so the
	// one glyph reads consistently as "here" throughout chorus's UI. A
	// continuation line (the 2nd+ line of a multi-line prompt) gets a
	// blank prompt of the same width instead of repeating "❯ ", so a
	// multi-line prompt doesn't read as several separate ones.
	ta.Prompt = "❯ "
	promptWidth := lipgloss.Width(ta.Prompt)
	ta.SetPromptFunc(promptWidth, func(line int) string {
		if line == 0 {
			return ta.Prompt
		}
		return strings.Repeat(" ", promptWidth)
	})
	promptStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	ta.FocusedStyle.Prompt = promptStyle
	ta.BlurredStyle.Prompt = promptStyle
	ta.ShowLineNumbers = false
	ta.Placeholder = `<agent>: text, a bare prompt to auto-route, /name ..., or !cmd to run directly — try "commands" (ctrl+j for a new line)`
	ta.KeyMap = inputKeyMap
	ta.CharLimit = 0
	ta.SetHeight(inputHeight)
	ta.Focus()

	// MouseWheelEnabled defaults true on viewport.Model — see main.go's
	// tea.WithMouseCellMotion() for why mouse events reach Update()'s
	// tea.MouseMsg case at all, and the tradeoff (native text selection
	// needs a terminal-level modifier-key override, e.g. Shift+drag on
	// Windows Terminal, once mouse capture is active) that decision
	// carries.
	vp := viewport.New(0, 0)

	return Model{
		ctx:              cfg.Ctx,
		renderer:         cfg.Renderer,
		coll:             cfg.Collectors,
		workers:          cfg.Workers,
		defaultAgent:     cfg.DefaultAgent,
		routing:          cfg.Routing,
		cwd:              cfg.Cwd,
		agentSpecs:       cfg.AgentSpecs,
		conns:            cfg.Conns,
		commands:         make(map[string][]acp.AvailableCommand),
		stats:            newAgentStats(),
		compaction:       cfg.Compaction,
		compactTriggered: make(map[string]bool),
		compactPending:   make(map[string]bool),
		outputCh:         cfg.OutputCh,
		permCh:           cfg.PermCh,
		errCh:            cfg.ErrCh,
		doneCh:           cfg.DoneCh,
		delegateLogCh:    cfg.DelegateLogCh,
		viewport:         vp,
		input:            ta,
		historyIndex:     -1,
		permMenuIndex:    -1,
		routeMenuIndex:   -1,
		contextMenuIndex: -1,
	}
}

// --- messages ---------------------------------------------------------

type outputMsg struct{ u bus.Update }
type outputClosedMsg struct{}
type permissionMsg struct{ req bus.PermissionRequest }
type errChMsg struct{ e ErrMsg }
type promptDoneMsg struct{ e PromptDoneMsg }

// routeDecisionMsg reports an LLM-based routing decision's result —
// success or failure, tagged with the reqID it was requested under (see
// Model.nextDecisionID's doc comment) and the original prompt text it was
// deciding for, since Update needs that to actually dispatch once the
// decision comes back.
type routeDecisionMsg struct {
	reqID    int
	prompt   string
	decision router.Decision
	err      error
}
type delegateLogMsg struct{ e delegate.LogEntry }
type spinnerTickMsg struct{}
type ctxDoneMsg struct{}

// --- Cmd generators (one blocking receive each, re-issued after every
// message so the channel keeps being drained) --------------------------

func waitForOutput(ch chan bus.Update) tea.Cmd {
	return func() tea.Msg {
		u, ok := <-ch
		if !ok {
			return outputClosedMsg{}
		}
		return outputMsg{u}
	}
}

func waitForPermission(ch chan bus.PermissionRequest) tea.Cmd {
	return func() tea.Msg {
		req, ok := <-ch
		if !ok {
			return nil
		}
		return permissionMsg{req}
	}
}

func waitForErr(ch chan ErrMsg) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return nil
		}
		return errChMsg{e}
	}
}

func waitForPromptDone(ch chan PromptDoneMsg) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return nil
		}
		return promptDoneMsg{e}
	}
}

func waitForDelegateLog(ch chan delegate.LogEntry) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return nil
		}
		return delegateLogMsg{e}
	}
}

func waitForCtxDone(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		<-ctx.Done()
		return ctxDoneMsg{}
	}
}

func spinnerTick() tea.Cmd {
	return tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

// --- bubbletea.Model ----------------------------------------------------

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		waitForOutput(m.outputCh),
		waitForPermission(m.permCh),
		waitForErr(m.errCh),
		waitForPromptDone(m.doneCh),
		waitForDelegateLog(m.delegateLogCh),
		waitForCtxDone(m.ctx),
		spinnerTick(),
		textarea.Blink,
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case outputMsg:
		return m.handleOutput(msg.u)

	case outputClosedMsg:
		// The output channel only closes when every agent Connection has
		// been torn down — nothing more will ever arrive.
		return m, nil

	case permissionMsg:
		// Deliberately NOT re-armed here — see waitForPermission's doc
		// comment and chorus-spec.md §7: while a permission is pending,
		// nothing reads permCh, so further requests queue in its
		// existing buffer (cap 4), reproducing the original REPL's
		// nil-channel-toggle FIFO behavior. Re-armed only from
		// answerPermission's success path below.
		p := msg.req
		m.pendingPerm = &p
		m.permCursor = 0
		m.renderer.ClearInPlaceState()
		m.blocks = append(m.blocks, block{text: m.renderer.FormatPermissionPrompt(p.Agent, p.Req, m.permCursor)})
		m.permMenuIndex = len(m.blocks) - 1
		m.syncViewport()
		return m, nil

	case errChMsg:
		m.appendLine(strings.TrimRight(formatErrLine(msg.e), "\n") + "\n")
		m.syncViewport()
		return m, waitForErr(m.errCh)

	case promptDoneMsg:
		// Only on success — a failed turn already gets its own error line
		// via errChMsg above; appending a second "finished in Xs" line for
		// the same event would just be noise on top of it. On success this
		// is the only place a completed turn is ever announced at all.
		if msg.e.Err == nil {
			m.appendLine(fmt.Sprintf("[%s] finished in %s\n", msg.e.Agent, formatDuration(msg.e.Duration)))
			m.syncViewport()
			// A usage update crossing threshold while this agent was mid-turn
			// deferred compaction rather than interrupting it — fire it now
			// that the turn (and the worker's busy state) has settled.
			if m.compactPending[msg.e.Agent] {
				m.fireCompaction(msg.e.Agent)
			}
		}
		return m, waitForPromptDone(m.doneCh)

	case routeDecisionMsg:
		return m.handleRouteDecision(msg)

	case delegateLogMsg:
		m.stats.delegateSent[msg.e.Source]++
		m.stats.delegateRecv[msg.e.Target]++
		if msg.e.Err != nil {
			m.stats.delegateFail[msg.e.Source]++
		}
		m.appendLine(formatDelegateLog(msg.e))
		m.syncViewport()
		return m, waitForDelegateLog(m.delegateLogCh)

	case nativeCmdResultMsg:
		m.appendLine(render.FormatNativeCommandResult(msg.cmdline, msg.output, msg.err, msg.duration))
		m.syncViewport()
		return m, nil

	case spinnerTickMsg:
		// uiTick animates the persistent "<agent> working (...)" status
		// line (View(), via render.SpinnerFrame) — advanced unconditionally,
		// unlike the in-document spinner below, since a background agent
		// can still be busy while a permission/routeAsk menu (or nothing at
		// all) currently owns the input.
		m.uiTick++
		// The in-document spinner (in-progress tool call / thinking
		// indicator) only animates while nothing else owns the input —
		// matches the original ticker case's "never disturb an active
		// permission/routeAsk prompt" gating.
		if m.mode() == modeNormal {
			if key, text, ok := m.renderer.FormatSpinnerTick(); ok {
				m.mergeBlock(key, text)
				m.syncViewport()
			}
		}
		return m, spinnerTick()

	case ctxDoneMsg:
		// Defense-in-depth (see the plan's Windows Ctrl+C risk callout) —
		// tea.KeyCtrlC below is the primary quit path once bubbletea owns
		// raw input, but ctx cancellation (e.g. a caught SIGINT before
		// the program grabbed the terminal) must still exit cleanly.
		m.quitting = true
		return m, tea.Quit

	case tea.WindowSizeMsg:
		return m.handleResize(msg), nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m Model) View() string {
	if !m.ready {
		return "starting up...\n"
	}
	// Two status lines, always both present (blank when inapplicable) —
	// see handleResize's "must match View() line-for-line" discipline.
	// Line 1 is the mode-related prompt (permission/routeAsk); line 2 is
	// the persistent busy-agents indicator, independent of mode, since a
	// background agent's turn can still be running while the user is
	// answering a totally different agent's permission prompt.
	var modeStatus string
	switch m.mode() {
	case modePermission:
		modeStatus = "awaiting permission answer (↑/↓ + enter, number, name, or \"cancel\")"
	case modeRouteAsk:
		modeStatus = "awaiting agent choice (↑/↓ + enter, number, or name)"
	case modeContextLevel:
		modeStatus = "choosing routing context level (↑/↓ + enter, number, or name)"
	}
	status := statusStyle.Render(modeStatus) + "\n" + statusStyle.Render(m.formatBusyStatus()) + "\n"
	return m.viewport.View() + "\n" + status + inputBoxStyle.Width(m.width).Render(m.input.View())
}

// formatBusyStatus renders one line listing every currently-busy agent with
// an animated spinner and elapsed time — e.g. "⠙ claude (12s)  ·  ⠙ opencode
// (3s)" — or "" if none are busy. Found live: with no such indicator, a
// long-running turn with no streamed output yet (an agent silently
// "thinking" before its first token) looked identical to nothing happening
// at all. Walks agentSpecs in registry order, the same determinism
// convention formatCapabilities/formatStats use, so this doesn't reorder
// from frame to frame as map iteration would.
func (m Model) formatBusyStatus() string {
	frame := render.SpinnerFrame(m.uiTick)
	var parts []string
	for _, spec := range m.agentSpecs {
		w, ok := m.workers[spec.Name]
		if !ok {
			continue
		}
		start, busy := w.StartedAt()
		if !busy {
			continue
		}
		parts = append(parts, fmt.Sprintf("%c %s (%s)", frame, spec.Name, formatDuration(time.Since(start))))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "  ·  ")
}

var (
	statusStyle = lipgloss.NewStyle().Faint(true)
	// inputBoxStyle draws a single top border above the input line — a
	// cheap, purely visual separator between the scrolling output and the
	// fixed input box, matching the "persistent input box" look the
	// design plan calls for. Width is set per-render from m.width so the
	// rule spans the full terminal, not just the typed text's length.
	inputBoxStyle = lipgloss.NewStyle().BorderStyle(lipgloss.NormalBorder()).BorderTop(true)
)

// --- Update() helpers ---------------------------------------------------

// handleOutput mirrors main.go's old outputCh case exactly: the delegate-
// collector check runs first (a sub-session's output is a private digest
// channel, never rendered here — see internal/delegate/collectors.go's
// doc comment), then the commands map is updated on
// AvailableCommandsUpdate, then the update is formatted and merged into
// the document.
func (m Model) handleOutput(u bus.Update) (tea.Model, tea.Cmd) {
	sid := u.Notification.SessionId
	if text, ok := extractAgentText(u); ok {
		if m.coll.Append(sid, text) {
			return m, waitForOutput(m.outputCh)
		}
		// A real interactive reply chunk (not a hidden sub-session's, per
		// the Append check above) — record it for routing.ContextLevel's
		// digest/full tiers (see Model.activity's doc comment).
		m.appendActivity("agent:"+u.Agent, text)
	} else if m.coll.IsRegistered(sid) {
		return m, waitForOutput(m.outputCh)
	}

	if acu := u.Notification.Update.AvailableCommandsUpdate; acu != nil {
		m.commands[u.Agent] = acu.AvailableCommands
	}

	// Tally direct mechanical work for the `stats` command (see agentStats'
	// doc comment) — ToolCall (not ToolCallUpdate) fires exactly once per
	// tool call, when it's first announced, so this can't double-count a
	// call across its later status updates. Excludes the delegate tool
	// itself: that's already counted precisely via delegateLogMsg below
	// (source/target, not just "claude made an other-kind call").
	if tc := u.Notification.Update.ToolCall; tc != nil {
		if !strings.HasPrefix(strings.ToLower(tc.Title), strings.ToLower(delegate.ToolTitlePrefix)) {
			m.stats.direct[u.Agent]++
		}
	}

	// Auto-compaction (chorus-spec.md §0's 2026-08-25 entry): reuses the
	// context-usage data render.FormatUpdate below already displays as a
	// dim "(tokens: used/size)" line, which used to be rendered and
	// otherwise discarded.
	if uu := u.Notification.Update.UsageUpdate; uu != nil {
		m.trackUsage(u.Agent, uu.Used, uu.Size)
	}

	if key, text, ok := m.renderer.FormatUpdate(u); ok {
		m.mergeBlock(key, text)
		m.syncViewport()
	}

	return m, waitForOutput(m.outputCh)
}

// trackUsage checks agent's freshly-reported context usage against
// compaction.Threshold() and fires (or defers, if agent is mid-turn)
// compaction once it's crossed — see compactTriggered/compactPending's
// doc comment on Model.
func (m *Model) trackUsage(agent string, used, size int) {
	if size <= 0 {
		return
	}
	pct := used * 100 / size
	if pct < m.compaction.Threshold() {
		delete(m.compactTriggered, agent)
		return
	}
	if !m.compaction.EnabledOrDefault() || m.compactTriggered[agent] {
		return
	}
	if w, ok := m.workers[agent]; ok && w.Idle() {
		m.fireCompaction(agent)
	} else {
		m.compactPending[agent] = true
	}
}

// fireCompaction discovers agent's own advertised compaction-like command
// (never a hardcoded "/compact" — see findCommandByAlias) and queues it
// as agent's next turn if found; if nothing matches, says so instead of
// guessing and sending a slash-string the agent may not actually support.
// Marks agent as triggered either way, so this doesn't re-fire on every
// subsequent usage update while still above threshold — trackUsage clears
// that once usage drops back down.
func (m *Model) fireCompaction(agent string) {
	cmd, ok := findCommandByAlias(m.commands[agent], m.compaction.AliasesOrDefault())
	if !ok {
		m.appendLine(fmt.Sprintf("[%s] crossed %d%% context usage but no compaction-like command was discovered — skipping\n", agent, m.compaction.Threshold()))
	} else if msg := queuePrompt(agent, "/"+cmd.Name, m.workers); msg != "" {
		m.appendLine(msg + "\n")
	}
	m.compactTriggered[agent] = true
	delete(m.compactPending, agent)
	m.syncViewport()
}

func (m *Model) handleResize(msg tea.WindowSizeMsg) Model {
	m.width, m.height = msg.Width, msg.Height
	m.ready = true

	// Must match View()'s exact layout line-for-line: viewport, then a
	// literal "\n" separator, then two always-reserved (even when blank
	// this frame) status lines, then inputBoxStyle's rendered output —
	// which is itself 1 (top border) + inputHeight (textarea content)
	// lines. Under-reserving here would make the input box (or its
	// border) get clipped off the bottom by the terminal itself.
	separatorHeight := 1
	statusHeight := 2
	inputBoxHeight := 1 + inputHeight // border line + textarea content lines
	vpHeight := m.height - separatorHeight - statusHeight - inputBoxHeight
	if vpHeight < 1 {
		vpHeight = 1
	}
	m.viewport.Width = m.width
	m.viewport.Height = vpHeight
	// textarea.Model.SetWidth accounts for its own Prompt width internally
	// (unlike textinput, which needed the prompt's width subtracted by
	// hand here) — see SetWidth's doc comment. The "-2" margin matches
	// what this line always reserved before the textinput->textarea swap.
	m.input.SetWidth(m.width - 2)

	m.renderer.SetWidth(m.width)
	m.syncViewport()

	return *m
}

// handleKey routes a keypress through m.mode()'s priority chain
// (permission answer > routeAsk answer > normal dispatch), exactly the
// original REPL's lineCh-handling order (main.go's old select loop).
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		m.quitting = true
		return m, tea.Quit
	}

	// Scroll keys are reserved for the viewport, not typed into the
	// input box, regardless of mode — a user should always be able to
	// scroll through history even while a permission/routeAsk prompt is
	// showing. pgup/pgdown/ctrl+u/ctrl+d go through viewport.Update
	// (bound in its DefaultKeyMap — ctrl+u/ctrl+d are half-page, kept as
	// a deliberate second, redundant path alongside pgup/pgdown: plain
	// control bytes like ctrl+u don't depend on a terminal correctly
	// translating a "special key" escape sequence the way pgup/pgdown
	// do, which is suspected — not yet confirmed, no way to test it from
	// this environment — as the cause of pgup/pgdown not scrolling at all
	// on at least one real Windows console (chorus-spec.md §0).
	//
	// home/end used to jump the viewport to top/bottom here, back when the
	// input was a single-line textinput with nowhere for a cursor to
	// usefully land. Now that it's a multi-line textarea, home/end are
	// needed for their far more standard job — start/end of the current
	// input line — so they're deliberately NOT intercepted here anymore;
	// they fall through below to the textarea, whose DefaultKeyMap already
	// binds home/end (and, for the same job, ctrl+a/ctrl+e — the readline-
	// style bindings macOS Terminal/iTerm's own line editing uses, so
	// nothing extra was needed to satisfy "appropriate keys on macOS").
	// Jumping the viewport to top/bottom is still reachable via pgup/pgdown.
	switch msg.String() {
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	// Arrow-key menu navigation, only while a permission or routeAsk
	// menu is actually up — otherwise up/down fall through to input-box
	// handling below. Typing a number/name still works exactly as before
	// regardless of whether the user has also been arrowing around — see
	// handlePermissionAnswer/handleRouteAnswer's empty-line-means-arrow-
	// selection fallback.
	if mode := m.mode(); mode != modeNormal {
		switch msg.Type {
		case tea.KeyUp:
			return m.moveMenuCursor(mode, -1), nil
		case tea.KeyDown:
			return m.moveMenuCursor(mode, 1), nil
		}
	}

	// Up/Down move the cursor within a multi-line prompt exactly like any
	// other multi-line editor (forwarded to the textarea below, unchanged)
	// — UNLESS the cursor is already at the very top/bottom display row of
	// the input, in which case there's nowhere further for it to go, and
	// the key instead recalls prompt history. "Top/bottom row" (not just
	// "first/last logical line") matters for a long single-line prompt
	// that's soft-wrapped across several rows — see atInputTop/
	// atInputBottom's own comments.
	if msg.Type == tea.KeyUp && m.atInputTop() {
		return m.historyUp(), nil
	}
	if msg.Type == tea.KeyDown && m.atInputBottom() {
		return m.historyDown(), nil
	}

	if msg.Type != tea.KeyEnter {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	line := strings.TrimSpace(m.input.Value())
	m.input.Reset()

	switch m.mode() {
	case modePermission:
		return m.handlePermissionAnswer(line)
	case modeRouteAsk:
		return m.handleRouteAnswer(line)
	case modeContextLevel:
		return m.handleContextLevelAnswer(line)
	default:
		return m.handleNormalLine(line)
	}
}

// atInputTop reports whether the input's cursor is on the topmost display
// row of the (possibly soft-wrapped) textarea content — i.e. there's
// nowhere for Up to move the cursor to, so it should recall history
// instead. Checking Line()==0 alone isn't enough: a single long line that
// wraps across several display rows keeps Line()==0 (a logical, not
// display, line index) until the cursor reaches the very top of that wrap,
// so RowOffset==0 is also required.
func (m Model) atInputTop() bool {
	return m.input.Line() == 0 && m.input.LineInfo().RowOffset == 0
}

// atInputBottom is atInputTop's mirror for Down — see its comment.
func (m Model) atInputBottom() bool {
	li := m.input.LineInfo()
	return m.input.Line() == m.input.LineCount()-1 && li.RowOffset == li.Height-1
}

// historyUp recalls the previous entry in promptHistory, saving the
// current (possibly partially-typed) input as historyDraft the first time
// it's called so paging back down past the newest entry restores it
// instead of leaving the input blank — the same convention a shell's
// readline history uses.
func (m Model) historyUp() Model {
	if len(m.promptHistory) == 0 {
		return m
	}
	switch {
	case m.historyIndex == -1:
		m.historyDraft = m.input.Value()
		m.historyIndex = len(m.promptHistory) - 1
	case m.historyIndex > 0:
		m.historyIndex--
	default:
		return m // already at the oldest entry
	}
	m.input.SetValue(m.promptHistory[m.historyIndex])
	return m
}

// historyDown is historyUp's mirror for Down — see its comment.
func (m Model) historyDown() Model {
	if m.historyIndex == -1 {
		return m // not currently navigating history
	}
	if m.historyIndex < len(m.promptHistory)-1 {
		m.historyIndex++
		m.input.SetValue(m.promptHistory[m.historyIndex])
		return m
	}
	m.historyIndex = -1
	m.input.SetValue(m.historyDraft)
	m.historyDraft = ""
	return m
}

// recordHistory appends a just-submitted line to promptHistory (skipping
// an exact repeat of the immediately preceding entry, so holding Up doesn't
// require paging through a run of duplicates to get past them) and resets
// history navigation state, exactly like a shell submitting a new command.
func (m *Model) recordHistory(line string) {
	if n := len(m.promptHistory); n == 0 || m.promptHistory[n-1] != line {
		m.promptHistory = append(m.promptHistory, line)
	}
	m.historyIndex = -1
	m.historyDraft = ""
}

// handlePermissionAnswer resolves an Enter press while a permission is
// pending. An empty line means the user navigated with arrow keys rather
// than typing — in that case the arrow-selected option (permCursor) is
// used directly, equivalent to typing its 1-based index; typed text (a
// number, name, or kind, or "cancel") always takes priority when present,
// exactly as before arrow-key selection existed.
func (m Model) handlePermissionAnswer(line string) (tea.Model, tea.Cmd) {
	if line == "" {
		line = strconv.Itoa(m.permCursor + 1)
	}
	if !answerPermission(m.pendingPerm, line) {
		m.appendLine("invalid choice, try again\n")
		m.syncViewport()
		return m, nil
	}
	m.pendingPerm = nil
	m.permMenuIndex = -1
	m.syncViewport()
	return m, waitForPermission(m.permCh)
}

// handleRouteAnswer is handlePermissionAnswer's routeAsk counterpart —
// same empty-line-means-arrow-selection fallback.
func (m Model) handleRouteAnswer(line string) (tea.Model, tea.Cmd) {
	names := m.pendingRoute.candidates
	if names == nil {
		names = agentNames(m.workers)
	}
	if line == "" {
		line = strconv.Itoa(m.routeCursor + 1)
	}
	agent, ok := matchName(line, names)
	if !ok {
		m.appendLine("invalid choice, try again\n")
		m.syncViewport()
		return m, nil
	}
	if msg := queuePrompt(agent, m.pendingRoute.text, m.workers); msg != "" {
		m.appendLine(msg + "\n")
	} else {
		m.appendLine(m.renderer.FormatUserPrompt(agent, m.pendingRoute.text))
		m.recordDispatch(agent, m.pendingRoute.text)
	}
	m.pendingRoute = nil
	m.routeMenuIndex = -1
	m.syncViewport()
	return m, nil
}

// handleContextLevelAnswer resolves an Enter press while the `context`
// command's menu is open — mirrors handleRouteAnswer/handlePermissionAnswer's
// empty-line-means-arrow-selection fallback.
func (m Model) handleContextLevelAnswer(line string) (tea.Model, tea.Cmd) {
	idx := m.contextCursor
	if line != "" {
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(contextLevels) {
			idx = n - 1
		} else if i := contextLevelIndexOf(line); i >= 0 {
			idx = i
		} else {
			m.appendLine("invalid choice, try again\n")
			m.syncViewport()
			return m, nil
		}
	}
	m.routing.ContextLevel = contextLevels[idx]
	m.contextMenuOpen = false
	m.contextMenuIndex = -1
	m.appendLine(fmt.Sprintf("routing context level set to %q\n", m.routing.ContextLevel))
	m.syncViewport()
	return m, nil
}

func (m Model) handleNormalLine(line string) (tea.Model, tea.Cmd) {
	if line == "" {
		return m, nil
	}
	m.recordHistory(line)
	if line == "quit" || line == "exit" {
		m.quitting = true
		return m, tea.Quit
	}
	if strings.HasPrefix(line, "!") {
		return m.handleNativeCommand(line)
	}
	if line == "thoughts" {
		m.renderer.ShowThoughts = !m.renderer.ShowThoughts
		m.appendLine("thoughts: " + onOff(m.renderer.ShowThoughts) + "\n")
		m.syncViewport()
		return m, nil
	}
	if line == "commands" {
		m.appendLine(formatCommands(m.commands))
		m.syncViewport()
		return m, nil
	}
	if line == "capabilities" {
		m.appendLine(formatCapabilities(m.agentSpecs, m.conns))
		m.syncViewport()
		return m, nil
	}
	if line == "stats" {
		m.appendLine(formatStats(m.agentSpecs, m.stats))
		m.syncViewport()
		return m, nil
	}
	if line == "context" {
		m.contextMenuOpen = true
		m.contextCursor = contextLevelIndexOf(m.routing.ContextLevelOrDefault())
		m.blocks = append(m.blocks, block{text: formatContextLevelMenu(m.contextCursor)})
		m.contextMenuIndex = len(m.blocks) - 1
		m.syncViewport()
		return m, nil
	}

	agent, sentText, msg, ask, decisionReq := dispatch(line, m.workers, m.defaultAgent, m.routing, m.commands)
	if msg != "" {
		m.appendLine(msg + "\n")
	}
	if agent != "" {
		// Echo what was actually asked — ACP never sends this back on a
		// live turn (only on session/load history replay), so without
		// this the only thing that ever appeared was the reply, with no
		// way to tell which reply answered which question. Found live.
		m.appendLine(m.renderer.FormatUserPrompt(agent, sentText))
		m.recordDispatch(agent, sentText)
	}
	if ask != nil {
		m.pendingRoute = ask
		m.routeCursor = 0
		m.blocks = append(m.blocks, block{text: formatRouteAskPrompt(ask, m.workers, m.routeCursor)})
		m.routeMenuIndex = len(m.blocks) - 1
	}
	if decisionReq != nil {
		return m.startRouteDecision(decisionReq.text)
	}
	m.syncViewport()
	return m, nil
}

// startRouteDecision kicks off an async LLM routing decision (dispatch
// returned a non-nil decisionReq — routing.mode is "llm" and no explicit
// "<agent>: "/slash-command path already resolved the prompt). Never
// blocks Model.Update: the actual decision call runs in runRouteDecision's
// tea.Cmd, on a hidden sub-session of the decision agent, reported back
// via routeDecisionMsg.
func (m Model) startRouteDecision(text string) (tea.Model, tea.Cmd) {
	conn, ok := m.conns[m.routing.DecisionAgent]
	if !ok {
		// Config error (decision_agent unset, misspelled, or not
		// connected) — don't cost a timeout on a call that can't succeed;
		// fall back to defaultAgent synchronously, same as routing "off".
		m.appendLine(fmt.Sprintf("routing.decision_agent %q isn't connected — falling back to %s\n", m.routing.DecisionAgent, m.defaultAgent))
		if errMsg := queuePrompt(m.defaultAgent, text, m.workers); errMsg != "" {
			m.appendLine(errMsg + "\n")
		} else {
			m.appendLine(m.renderer.FormatUserPrompt(m.defaultAgent, text))
			m.recordDispatch(m.defaultAgent, text)
		}
		m.syncViewport()
		return m, nil
	}

	m.nextDecisionID++
	reqID := m.nextDecisionID
	agents := decisionAgentInfos(m.agentSpecs, m.workers)
	contextText := m.activityContext(m.routing.ContextLevelOrDefault())
	m.appendLine(fmt.Sprintf("[routing] asking %s to decide...\n", m.routing.DecisionAgent))
	m.syncViewport()
	// The decision sub-session must be opened with the decision agent's OWN
	// effective cwd (conn.Cwd() — the in-container mount point like
	// "/workspace" for a sandboxed agent), NOT the raw host cwd. See
	// Connection.Cwd's doc comment: passing m.cwd silently broke every
	// routing decision to a containerized decision agent (session/new
	// accepts the bad cwd, then session/prompt fails "-32603").
	return m, runRouteDecision(m.ctx, conn, m.coll, conn.Cwd(), agents, m.defaultAgent, contextText, text, reqID, m.routing.DecisionTimeout())
}

// handleRouteDecision resolves a routeDecisionMsg: picks the target agent
// (the decision, if it parsed and validated; otherwise defaultAgent),
// attempts a model switch if the decision named one and the target
// advertises a matching command, prepends a one-time handoff preamble if
// the target differs from lastRoutedAgent, then dispatches exactly like
// any other successful path.

func (m Model) handleRouteDecision(msg routeDecisionMsg) (tea.Model, tea.Cmd) {
	target := m.defaultAgent
	var modelID string
	if msg.err != nil {
		m.appendLine(fmt.Sprintf("[routing] decision failed (%v) — falling back to %s\n", msg.err, m.defaultAgent))
	} else if knownAgent(msg.decision.Agent, m.workers) {
		target = msg.decision.Agent
		modelID = msg.decision.Model
	} else {
		m.appendLine(fmt.Sprintf("[routing] decision named %q, which isn't connected — falling back to %s\n", msg.decision.Agent, m.defaultAgent))
	}
	if target == "" || !knownAgent(target, m.workers) {
		m.appendLine(fmt.Sprintf("no default agent configured or connected — routing failed for: %s\n", msg.prompt))
		m.syncViewport()
		return m, nil
	}

	if modelID != "" {
		if cmd, ok := findCommandByAlias(m.commands[target], modelSwitchAliases); ok {
			if errMsg := queuePrompt(target, "/"+cmd.Name+" "+modelID, m.workers); errMsg != "" {
				m.appendLine(errMsg + "\n")
			}
		}
		// No matching command discovered: silently skip the model switch
		// (not an error worth a line — routing to the right AGENT is the
		// important part, and not every agent advertises model switching
		// at all, believed to be the common case, not the exception).
	}

	promptText := msg.prompt
	if m.lastRoutedAgent != "" && m.lastRoutedAgent != target {
		promptText = buildHandoffPreamble(m.lastRoutedAgent, m.activityContext("full")) + "\n\n" + msg.prompt
	}
	if errMsg := queuePrompt(target, promptText, m.workers); errMsg != "" {
		m.appendLine(errMsg + "\n")
	} else {
		m.appendLine(m.renderer.FormatUserPrompt(target, msg.prompt))
		m.recordDispatch(target, msg.prompt)
	}
	m.syncViewport()
	return m, nil
}

// modelSwitchAliases are matched (findCommandByAlias, substring,
// case-insensitive) against an agent's advertised commands to discover a
// model-switch command — never hardcoded as a literal "/model", since
// whether any agent exposes this at all, and under what name, is
// unconfirmed across the board (chorus-spec.md §0).
var modelSwitchAliases = []string{"model", "switch-model"}

// contextLevels are routing.ContextLevel's three valid values, in the
// order the `context` command's menu displays them.
var contextLevels = []string{"prompt", "digest", "full"}

// contextLevelIndexOf returns level's index in contextLevels, or 1
// ("digest", the overall default) if level doesn't match any of them.
func contextLevelIndexOf(level string) int {
	for i, l := range contextLevels {
		if l == level {
			return i
		}
	}
	return 1
}

// formatContextLevelMenu renders the `context` command's arrow-key menu —
// same render.FormatMenuLine convention as the permission/routeAsk menus.
func formatContextLevelMenu(cursor int) string {
	var b strings.Builder
	b.WriteString("\nrouting context level (how much recent activity is sent to the LLM routing decision):\n")
	for i, lvl := range contextLevels {
		b.WriteString(render.FormatMenuLine(i, cursor, lvl))
	}
	b.WriteString("↑/↓ + enter, or type a number/name\n")
	return b.String()
}

// recordDispatch appends the just-sent prompt to the activity log and
// updates lastRoutedAgent — called from every successful dispatch path
// (explicit prefix, slash command, routing "off", or an LLM routing
// decision) so a later routing decision's handoff detection is accurate
// regardless of how the previous turn was routed.
func (m *Model) recordDispatch(agent, text string) {
	m.appendActivity("user:"+agent, text)
	m.lastRoutedAgent = agent
}

// activityLogCap/activityDigestN/activityEntryCap bound Model.activity —
// see its doc comment. Entry count (not chunk count, thanks to
// appendActivity's merge-by-key behavior) is bounded so "full" context
// still can't grow unboundedly on a very long session, which would
// undercut the whole reason a cheaper "digest" tier exists.
const (
	activityLogCap   = 60
	activityDigestN  = 8
	activityEntryCap = 1000
)

// appendActivity records one turn's worth of activity, merging into the
// last entry if it shares the same key (role:agent) — e.g. multiple
// streamed AgentMessageChunks from the same agent's same reply become one
// entry, not one per chunk, so the log's entry count reflects turns, not
// arbitrary streaming granularity.
func (m *Model) appendActivity(key, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if n := len(m.activity); n > 0 && m.activity[n-1].key == key {
		merged := m.activity[n-1].text + text
		if len(merged) > activityEntryCap {
			merged = merged[:activityEntryCap] + "..."
		}
		m.activity[n-1].text = merged
		return
	}
	m.activity = append(m.activity, activityEntry{key: key, text: text})
	if len(m.activity) > activityLogCap {
		m.activity = m.activity[len(m.activity)-activityLogCap:]
	}
}

func formatActivity(entries []activityEntry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "[%s] %s\n", e.key, e.text)
	}
	return b.String()
}

// activityContext renders Model.activity at the given tier — "prompt"
// (nothing), "digest" (the last activityDigestN entries — the cheap,
// mechanical default: no extra LLM call, so the router doesn't undercut
// its own cost-saving purpose by default), or "full" (everything still
// retained, capped by activityLogCap). Unrecognized levels fall back to
// "digest".
func (m Model) activityContext(level string) string {
	switch level {
	case "prompt":
		return ""
	case "full":
		return formatActivity(m.activity)
	default:
		n := activityDigestN
		if n > len(m.activity) {
			n = len(m.activity)
		}
		return formatActivity(m.activity[len(m.activity)-n:])
	}
}

// handleNativeCommand handles a "!<cmdline>" input line — chorus's native,
// agent-free command execution (see runNativeCommand's doc comment for the
// full rationale). Echoes the command immediately (same "echo what was
// actually sent" convention handleNormalLine's dispatch path uses for
// agent prompts) and kicks off async execution via a tea.Cmd — never run
// synchronously here, since Update() must stay free to keep servicing
// outputCh/permCh for every agent while the command runs, exactly the
// discipline AgentWorker exists for on the agent-prompt side.
func (m Model) handleNativeCommand(line string) (tea.Model, tea.Cmd) {
	cmdline := strings.TrimSpace(strings.TrimPrefix(line, "!"))
	if cmdline == "" {
		m.appendLine("usage: !<shell command>  (runs directly on this machine, no agent involved)\n")
		m.syncViewport()
		return m, nil
	}
	m.appendLine(m.renderer.FormatNativeCommandEcho(cmdline))
	m.syncViewport()
	return m, runNativeCommand(m.ctx, cmdline)
}

// mergeBlock implements the document's merge rule: a mergeable block
// (key != "") overwrites the current last block if it shares that same
// key; otherwise (including key == "" one-shot blocks) it's appended as a
// new block. This is beginRedraw/endRedraw's "same key as immediately
// preceding -> overwrite; different key -> finalize and start fresh"
// rule, operating on a slice instead of ANSI cursor movement.
func (m *Model) mergeBlock(key, text string) {
	if key != "" && len(m.blocks) > 0 && m.blocks[len(m.blocks)-1].key == key {
		m.blocks[len(m.blocks)-1].text = text
		return
	}
	m.blocks = append(m.blocks, block{key: key, text: text})
}

// moveMenuCursor advances the current menu's arrow-key cursor by delta
// (wrapping at either end) and re-renders that exact block in place via
// permMenuIndex/routeMenuIndex — not mergeBlock, since a long-pending
// menu can easily no longer be the last block by the time an arrow key
// arrives (another agent's unrelated output may have streamed in around
// it in the meantime).
func (m Model) moveMenuCursor(mode inputMode, delta int) Model {
	switch mode {
	case modePermission:
		n := len(m.pendingPerm.Req.Options)
		if n == 0 {
			return m
		}
		m.permCursor = wrapIndex(m.permCursor+delta, n)
		m.setBlockAt(m.permMenuIndex, m.renderer.FormatPermissionPrompt(m.pendingPerm.Agent, m.pendingPerm.Req, m.permCursor))
	case modeRouteAsk:
		names := m.pendingRoute.candidates
		if names == nil {
			names = agentNames(m.workers)
		}
		n := len(names)
		if n == 0 {
			return m
		}
		m.routeCursor = wrapIndex(m.routeCursor+delta, n)
		m.setBlockAt(m.routeMenuIndex, formatRouteAskPrompt(m.pendingRoute, m.workers, m.routeCursor))
	case modeContextLevel:
		m.contextCursor = wrapIndex(m.contextCursor+delta, len(contextLevels))
		m.setBlockAt(m.contextMenuIndex, formatContextLevelMenu(m.contextCursor))
	default:
		return m
	}
	m.syncViewport()
	return m
}

// wrapIndex adds delta to i and wraps into [0, n) — so pressing down at
// the last option moves to the first, and vice versa, rather than
// getting stuck at either end.
func wrapIndex(i, n int) int {
	if n == 0 {
		return 0
	}
	i %= n
	if i < 0 {
		i += n
	}
	return i
}

// setBlockAt overwrites the block at idx directly, if it's still a valid
// index — see moveMenuCursor's doc comment for why this bypasses
// mergeBlock.
func (m *Model) setBlockAt(idx int, text string) {
	if idx >= 0 && idx < len(m.blocks) {
		m.blocks[idx].text = text
	}
}

// appendLine appends a one-shot (never merged) block — the equivalent of
// the old REPL's bare fmt.Print* calls for transient/informational text.
func (m *Model) appendLine(text string) {
	m.mergeBlock("", text)
}

// syncViewport re-renders the full document into the viewport.
//
// Auto-follow scrolling (genuinely new behavior — the line-based REPL
// always appended at the live cursor, which doesn't apply to a scrollable
// viewport): capture atBottom BEFORE SetContent, call GotoBottom after
// only if it was true beforehand. Getting this order right matters —
// reversed, the view yanks back to bottom while a user is scrolling
// through history during active streaming.
//
// Exception: while a permission or routeAsk menu is pending (m.mode() !=
// modeNormal), GotoBottom fires unconditionally instead of only when
// atBottom was already true. Found live: a user who had scrolled up even
// slightly when a menu appeared (or who scrolled away while it was
// already showing) would have it render partially or entirely off-screen
// — arrow-key cursor movement doesn't scroll the viewport (down/up are
// captured for menu selection instead, see handleKey), so nothing short
// of pgup/pgdn/home/end could bring it back into view, and the user has
// no way to answer a question they can't see. A pending menu blocks all
// forward progress until it's answered, so unlike ordinary streamed
// output it must always win over the user's scroll position — every
// syncViewport call while one is pending (menu creation, cursor move,
// an "invalid choice" reprompt, or even unrelated output from another
// agent streaming in around it) re-pins the view to the bottom until the
// menu is resolved.
func (m *Model) syncViewport() {
	atBottom := m.viewport.AtBottom()
	m.viewport.SetContent(m.renderDocument())
	if atBottom || m.mode() != modeNormal {
		m.viewport.GotoBottom()
	}
}

// renderDocument concatenates every block's text in order (each already
// carries its own trailing newline where one belongs, exactly as before
// when blocks were fmt.Fprint'd directly to stdout one after another),
// then word-wraps the whole thing to the viewport's width. The wrap is
// necessary because bubbles/viewport truncates (does not wrap) lines
// wider than its width — unlike a real terminal, which always soft-wraps
// overflow. Markdown blocks are already wrapped to this same width via
// renderer.SetWidth, so the wrap here is close to a no-op for them; it's
// what keeps un-wrapped blocks (tool call lines, diffs, plan checklists,
// permission prompts) from silently losing content instead of just
// looking different, which the plain-REPL version never risked.
func (m Model) renderDocument() string {
	var b strings.Builder
	for _, blk := range m.blocks {
		b.WriteString(blk.text)
	}
	doc := strings.TrimRight(b.String(), "\n")
	if m.viewport.Width <= 0 {
		return doc
	}
	return wordwrap.String(doc, m.viewport.Width)
}

// StripANSI applies to e.Err.Error() even though most such errors are
// Go-level connection/protocol failures, not agent-authored text —
// chorus-spec.md/CLAUDE.md's security invariant #3 is "every site that
// interpolates text possibly influenced by an agent," and a JSON-RPC
// error's message can in principle echo back agent/subprocess-controlled
// content (e.g. a rejected-prompt error including part of what was
// rejected). Cheap to apply defensively; found by an explicit
// invariant-by-invariant audit, not a live incident.
func formatErrLine(e ErrMsg) string {
	return "\n[" + e.Agent + "] error: " + render.StripANSI(e.Err.Error()) + "\n"
}
