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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/atotto/clipboard"
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
	// usageUsed/usageSize hold each agent's MOST RECENTLY reported
	// context usage (ACP's usage_update — the same data trackUsage
	// already consumed for auto-compaction, previously discarded once
	// that check ran) — added 2026-09-04 so `stats` can answer "how many
	// tokens has each agent used so far" as of right now, not just at
	// whatever moment a usage_update happened to stream past on screen.
	// A snapshot, not a running total across turns — ACP's own Used
	// value is already cumulative for the session, so overwriting on
	// each update (not summing) is correct.
	usageUsed map[string]int
	usageSize map[string]int
}

func newAgentStats() agentStats {
	return agentStats{
		direct:       make(map[string]int),
		delegateSent: make(map[string]int),
		delegateRecv: make(map[string]int),
		delegateFail: make(map[string]int),
		usageUsed:    make(map[string]int),
		usageSize:    make(map[string]int),
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
	// ImageDir is where a Ctrl+V clipboard-image paste is saved before
	// being referenced as an ordinary "@path" attachment — the same
	// directory main.go already creates for inbound agent-sent images, so
	// a session's images (sent or received) all live in one place. ""
	// falls back to os.TempDir() rather than erroring.
	ImageDir string

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
	imageDir     string

	// activity is a merged-by-turn log of what's happened in the interactive
	// session — the mechanical (no extra LLM call) backing for all three
	// routing.ContextLevel tiers (chorus-spec.md §0's 2026-08-25 entry):
	// "prompt" skips it, "digest" takes the last few entries (truncated,
	// cheap), "full" takes the whole thing within a char budget. Entries are
	// stored near-verbatim (activityEntryCap is high, only a memory
	// backstop); truncation is applied at RENDER, per tier — so the two jobs
	// this log serves stay decoupled: the router's decision prompt stays
	// small (digest), while a cross-agent handoff, which is built from the
	// "full" tier, carries the real prior work. Found live: at the old flat
	// 1000-char/entry cap a handoff dropped everything past ~Phase 1 of a
	// plan the previous agent had written. lastRoutedAgent is which agent
	// most recently received a prompt, by ANY dispatch path (explicit
	// prefix, slash command, or LLM routing decision) — used to detect an
	// actual agent switch worth a handoff, regardless of how it happened.
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

	// autoPrevMode remembers, per agent, whichever ACP session mode it was
	// in immediately before the `auto` REPL command switched it into
	// spec.AutoMode — so a second `auto`/`auto <agent>` toggles back to
	// that instead of just leaving the agent stuck in auto mode forever.
	// Absence of an entry means "not currently in auto mode via this
	// command" (distinct from CurrentModeId itself, which an agent can
	// also change on its own via a current_mode_update — see
	// handleOutput's CurrentModeUpdate case).
	autoPrevMode map[string]acp.SessionModeId

	outputCh      chan bus.Update
	permCh        chan bus.PermissionRequest
	errCh         chan ErrMsg
	doneCh        chan PromptDoneMsg
	delegateLogCh chan delegate.LogEntry

	viewport viewport.Model
	input    textarea.Model

	// selecting/selectAnchorLine/selectCurLine back click-drag text
	// selection + copy-on-select — see handleMouse's doc comment for why
	// chorus needs to implement this itself rather than relying on the
	// terminal: tea.WithMouseCellMotion() (main.go) already takes over
	// plain click-drag for scrolling, so a left-button drag inside the
	// viewport never reaches the terminal's own selection at all unless
	// a user holds a modifier key. Deliberately line-level, not
	// character-column: both endpoints are absolute line indices into
	// the current word-wrapped document (m.viewport.YOffset + the
	// clicked row), not column offsets — selecting always copies whole
	// lines. This avoids tracking exact rune/column positions through
	// wordwrap's reflow and glamour's ANSI styling (materially more
	// complex than a line-oriented selection over a scrolling terminal
	// buffer needs to be) at the cost of not supporting a mid-line-to-
	// mid-line selection — a deliberate, documented trade-off, not an
	// oversight.
	selecting        bool
	selectAnchorLine int
	selectCurLine    int
	// selectionActive is whether [selectAnchorLine, selectCurLine] should
	// currently be rendered with a highlight (applySelectionHighlight) —
	// separate from `selecting` (which specifically means "mouse button
	// currently held, motion events should keep extending the range").
	// True from the initial press through release AND for a while after
	// (so the just-copied text stays visibly marked, the same lifespan as
	// lastCopyStatus's confirmation message below), cleared on the next
	// keypress. Found live (2026-09, user report): without any highlight
	// at all, a click-drag copied the right text to the clipboard but
	// looked like nothing happened on screen — the copy worked, but a
	// user watching the screen had no visual confirmation anything was
	// selected while dragging, which reads as broken even though it
	// wasn't.
	selectionActive bool
	// lastCopyStatus is a one-shot confirmation ("3 lines copied" / a
	// clipboard error) shown on the mode-status line in View() right
	// after a selection completes, then cleared on the next keypress or
	// selection — copy-on-select has no other feedback channel, since it
	// doesn't append to the document (that would itself change what's
	// on screen, undermining the very selection the user just made).
	lastCopyStatus string

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

	// pastes backs the large-paste collapse feature ("[Pasted text #N
	// +NN lines]"): keyed by the chip inserted into the textarea, valued
	// by the full pasted text. expandPastes resolves a chip back to its
	// full text at the actual dispatch choke point, so the input box,
	// scrollback echo, and promptHistory all keep showing the short form.
	// Never cleared — a chip recalled from promptHistory later must still
	// expand correctly.
	pastes      map[string]string
	nextPasteID int

	// suggestKind/Items/Cursor/TokenStart/Token back the live "/"-command
	// and "@"-file completion popup, recomputed once per keypress
	// (refreshSuggest) and cached here so View()/relayout() only ever
	// read — never recompute — and so can't disagree about the popup's
	// size for a given keystroke. suggestToken is the token the cached
	// items were computed for, used to detect "filter changed, reset the
	// cursor." suggestSuppressed/SuppressedToken implement Esc-to-dismiss
	// for exactly that one token — typing further naturally un-suppresses.
	suggestKind            suggestKind
	suggestItems           []suggestItem
	suggestCursor          int
	suggestTokenStart      int
	suggestToken           string
	suggestSuppressed      bool
	suggestSuppressedToken string

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

	// focused tracks terminal focus (tea.FocusMsg/BlurMsg, enabled via
	// main.go's tea.WithReportFocus()) — defaults true so a terminal that
	// never reports focus at all (the option is a no-op if unsupported)
	// never rings a bell it was never asked to, rather than bell-spamming
	// every single turn. Backs bellSuffix's "notify only while the user
	// probably isn't looking" behavior.
	focused bool
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
	// marker at all for "this is where you type"). "❯ " matches the same
	// cursor glyph the arrow-key menu selection already uses
	// (render.FormatMenuLine), so the one glyph reads consistently as
	// "here" throughout chorus's UI. A
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
		imageDir:         cfg.ImageDir,
		commands:         make(map[string][]acp.AvailableCommand),
		stats:            newAgentStats(),
		compaction:       cfg.Compaction,
		compactTriggered: make(map[string]bool),
		compactPending:   make(map[string]bool),
		autoPrevMode:     make(map[string]acp.SessionModeId),
		outputCh:         cfg.OutputCh,
		permCh:           cfg.PermCh,
		errCh:            cfg.ErrCh,
		doneCh:           cfg.DoneCh,
		delegateLogCh:    cfg.DelegateLogCh,
		viewport:         vp,
		input:            ta,
		focused:          true,
		pastes:           make(map[string]string),
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
		m.appendLine(strings.TrimRight(formatErrLine(msg.e), "\n") + "\n" + m.bellSuffix())
		m.syncViewport()
		return m, waitForErr(m.errCh)

	case promptDoneMsg:
		// Only on success — a failed turn already gets its own error line
		// via errChMsg above; appending a second "finished in Xs" line for
		// the same event would just be noise on top of it. On success this
		// is the only place a completed turn is ever announced at all.
		if msg.e.Err == nil {
			m.appendLine(fmt.Sprintf("[%s] finished in %s\n", msg.e.Agent, formatDuration(msg.e.Duration)) + m.bellSuffix())
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

	case setModeResultMsg:
		if msg.err != nil {
			m.appendLine(fmt.Sprintf("%s: mode switch failed: %v\n", msg.agent, msg.err))
			m.syncViewport()
			return m, nil
		}
		// Belt-and-suspenders update, same reasoning as AgentSession.SetMode's
		// own doc comment: an agent's current_mode_update notification is
		// still the source of truth (handleOutput's CurrentModeUpdate case),
		// but not every agent is confirmed to send one after every switch.
		if w, ok := m.workers[msg.agent]; ok && w.sess != nil {
			w.sess.CurrentModeId = msg.modeId
		}
		if msg.auto {
			if msg.turnOn {
				m.autoPrevMode[msg.agent] = msg.restoreId
				m.appendLine(fmt.Sprintf("%s: auto mode on (%s)\n", msg.agent, msg.modeLabel))
			} else {
				delete(m.autoPrevMode, msg.agent)
				m.appendLine(fmt.Sprintf("%s: auto mode off\n", msg.agent))
			}
		} else {
			m.appendLine(fmt.Sprintf("%s: mode set to %s\n", msg.agent, msg.modeLabel))
		}
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
		return m.handleMouse(msg)

	case tea.FocusMsg:
		m.focused = true
		return m, nil

	case tea.BlurMsg:
		m.focused = false
		return m, nil
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
	// lastCopyStatus only ever shows on this line when nothing else claims
	// it — a pending permission/routeAsk/contextLevel prompt always wins,
	// since answering that is the more urgent thing on screen.
	if modeStatus == "" && m.lastCopyStatus != "" {
		modeStatus = m.lastCopyStatus
	}
	status := statusStyle.Render(modeStatus) + "\n" + statusStyle.Render(m.formatBusyStatus()) + "\n"
	return m.viewport.View() + "\n" + status + m.renderSuggestOverlay() + inputBoxStyle.Width(m.width).Render(m.input.View())
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
	// "esc to interrupt" only makes sense to show here, not as a permanent
	// hint elsewhere, since it's only actually actionable while something
	// is busy — surfacing Esc's new job (interruptBusyAgents) right where
	// the user is already watching a turn run, rather than leaving them to
	// discover it only by reading docs or by accident.
	return strings.Join(parts, "  ·  ") + "   (esc to interrupt)"
}

// bellSuffix returns a terminal bell character when the terminal is known
// to be unfocused, "" otherwise — appended to a turn's completion/error
// line so it rides through the normal View()-rendered document (concurrency
// invariant #3: nothing outside bubbletea's own render cycle may write to
// the terminal) rather than needing a separate raw write. Deliberately
// silent while focused ("tell me when I've looked away, not on every
// turn") rather than bell-spamming a user who's actively watching the
// screen.
func (m Model) bellSuffix() string {
	if m.focused {
		return ""
	}
	return "\a"
}

// handleMouse implements click-drag text selection + copy-on-select:
// tea.WithMouseCellMotion() (main.go) puts chorus, not the terminal, in
// charge of mouse events, which means a plain click-drag never reaches
// the terminal's native selection at all unless a user knows to hold a
// modifier key (Shift on most terminals) — something CHORUS_DISABLE_MOUSE
// documents as the escape hatch, but shouldn't be the ONLY way to select
// and copy text, since that's a basic, expected terminal-app capability.
//
// Only left-button events participate; wheel events fall through to
// viewport.Update unchanged (its own MouseWheelEnabled path, untouched by
// any of this). A press outside the viewport's rows (the status lines or
// the input box) doesn't start a selection — the input box already owns
// its own click/cursor behavior via bubbles/textarea, and there is nothing
// selectable in an empty status line.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Button == tea.MouseButtonLeft {
		switch msg.Action {
		case tea.MouseActionPress:
			if msg.Y >= 0 && msg.Y < m.viewport.Height {
				m.selecting = true
				m.selectionActive = true
				m.lastCopyStatus = ""
				line := m.viewport.YOffset + msg.Y
				m.selectAnchorLine = line
				m.selectCurLine = line
				m.syncViewport()
			} else {
				m.selecting = false
			}
			return m, nil

		case tea.MouseActionMotion:
			if m.selecting {
				m.selectCurLine = m.viewport.YOffset + clampInt(msg.Y, 0, m.viewport.Height-1)
				m.syncViewport()
			}
			return m, nil

		case tea.MouseActionRelease:
			if m.selecting {
				m.selecting = false
				m = m.copySelection()
				m.syncViewport()
				return m, nil
			}
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

// copySelection extracts the lines between selectAnchorLine and
// selectCurLine (inclusive, order-independent) from the current document,
// strips ANSI styling (a terminal clipboard should receive plain text, not
// chorus's own color codes), and writes the result to the system clipboard
// via github.com/atotto/clipboard — which already implements the
// necessary per-platform mechanisms (pbcopy on macOS, wl-copy/xclip/xsel
// on Linux depending on the session type, the native Win32 clipboard API
// on Windows with no external process needed). Recomputes the document
// fresh (renderDocument is a pure,
// cheap function of m.blocks/m.viewport.Width) rather than reading from a
// cache, so a selection made while output is actively streaming in still
// reflects exactly what was on screen at release time.
//
// Deliberately does not fall back to OSC 52 (a terminal escape sequence
// that also updates the clipboard, notably useful over SSH where none of
// the local utilities above could reach the user's own machine): writing
// it would mean putting raw bytes on os.Stdout outside of bubbletea's own
// render cycle, which is exactly what concurrency invariant #3 (CLAUDE.md)
// exists to prevent — a corrupted frame is a worse failure mode than "copy
// silently didn't work over this SSH session," which lastCopyStatus's error
// message at least surfaces honestly instead of pretending to succeed.
func (m Model) copySelection() Model {
	text, n, ok := extractSelection(m.renderDocument(), m.selectAnchorLine, m.selectCurLine)
	if !ok {
		return m
	}
	if err := writeClipboard(text); err != nil {
		m.lastCopyStatus = fmt.Sprintf("copy failed: %v", err)
		return m
	}
	if n == 1 {
		m.lastCopyStatus = "1 line copied"
	} else {
		m.lastCopyStatus = fmt.Sprintf("%d lines copied", n)
	}
	return m
}

// extractSelection returns the ANSI-stripped, newline-joined text of doc's
// lines between anchor and cur (inclusive, order-independent — a drag can
// go either direction), plus how many lines that was. Pulled out of
// copySelection as its own pure function specifically so the actual line-
// range/ANSI-stripping logic is unit-testable without touching the real
// OS clipboard (see writeClipboard). ok is false only for a degenerate
// empty range — not reachable from a real mouse drag inside the viewport,
// but checked rather than assumed.
func extractSelection(doc string, anchor, cur int) (text string, lineCount int, ok bool) {
	lo, hi := anchor, cur
	if lo > hi {
		lo, hi = hi, lo
	}
	lines := strings.Split(doc, "\n")
	if lo < 0 {
		lo = 0
	}
	if hi >= len(lines) {
		hi = len(lines) - 1
	}
	if lo > hi {
		return "", 0, false
	}
	selected := make([]string, 0, hi-lo+1)
	for _, l := range lines[lo : hi+1] {
		selected = append(selected, render.StripANSI(l))
	}
	return strings.Join(selected, "\n"), hi - lo + 1, true
}

// writeClipboard is a package-level function variable (not a direct call
// to clipboard.WriteAll) specifically so tests can substitute a fake — the
// real implementation reaches the actual OS clipboard via per-platform
// mechanisms (pbcopy/wl-copy/xclip/xsel/the Win32 clipboard API), which
// may not even be available in a headless test environment (e.g. Linux CI
// with no X11/Wayland session), the same reason internal/session's live
// subprocess-dependent code stays outside this package's unit test
// coverage.
var writeClipboard = clipboard.WriteAll

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// interruptBusyAgents implements Esc's "stop the current turn without
// quitting the program" behavior (2026-09-02): previously the only way to
// stop a running turn was Ctrl+C, which kills every connected agent's
// subprocess at once, mid-turn, discarding whatever any OTHER agent was
// doing too — not just the one turn the user actually wanted to stop.
//
// Deciding WHICH agent Esc interrupts only matters because chorus runs
// several agents at once — resolved by picking lastRoutedAgent (the most
// recently dispatched turn — the one the user is most likely watching) if
// it's currently busy; otherwise every currently-busy agent is interrupted,
// so Esc is never a silent no-op just because the most recent dispatch
// already finished while some earlier/background turn (e.g. a slow
// delegate reply) is still running.
//
// Interrupting does not end the session: AgentWorker.Cancel sends ACP's own
// session/cancel notification, and the worker's in-flight PromptContent
// call is expected to return (successfully, just early) exactly as it does
// for any normally-completed turn — reported via the same doneCh/errCh path,
// with a "[agent] finished in Xs" line following shortly after, same as
// always.
func (m Model) interruptBusyAgents() Model {
	targets := selectInterruptTargets(m.busyAgentNames(), m.lastRoutedAgent)
	if len(targets) == 0 {
		return m
	}
	for _, name := range targets {
		w, ok := m.workers[name]
		if !ok {
			continue
		}
		if err := w.Cancel(m.ctx); err != nil {
			m.appendLine(fmt.Sprintf("[%s] interrupt failed: %v\n", name, err))
			continue
		}
		m.appendLine(fmt.Sprintf("[%s] interrupted\n", name))
	}
	m.syncViewport()
	return m
}

// busyAgentNames returns every currently-busy agent's name, in agentSpecs
// (registry) order for determinism — the same convention
// formatCapabilities/formatStats/formatBusyStatus already use.
func (m Model) busyAgentNames() []string {
	var names []string
	for _, spec := range m.agentSpecs {
		if w, ok := m.workers[spec.Name]; ok && !w.Idle() {
			names = append(names, spec.Name)
		}
	}
	return names
}

// selectInterruptTargets narrows a list of busy agent names down to which
// one(s) Esc should actually interrupt — pulled out of interruptBusyAgents
// as its own pure function specifically so this decision is unit-testable
// without a live ACP connection (AgentWorker.Cancel's underlying
// session.AgentSession.Cancel needs a real *acp.ClientSideConnection, which
// only a live subprocess provides — see CLAUDE.md on internal/session
// remaining otherwise untested for the same reason). If lastRouted is
// currently busy, it alone is targeted (the turn the user is most likely
// watching); otherwise every busy agent is targeted, so Esc is never a
// silent no-op just because the most recently dispatched turn already
// finished while some other agent is still running.
func selectInterruptTargets(busy []string, lastRouted string) []string {
	for _, name := range busy {
		if name == lastRouted {
			return []string{name}
		}
	}
	return busy
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

	// Keep AgentSession.CurrentModeId in sync with the agent's own
	// current_mode_update notification — the agent's mode changing on
	// its own (e.g. its own /plan-style command) isn't only something
	// chorus's `mode`/`auto` commands cause. See AgentSession's own doc
	// comment on why this notification, not the SetMode request itself,
	// is treated as the source of truth.
	if cmu := u.Notification.Update.CurrentModeUpdate; cmu != nil {
		if w, ok := m.workers[u.Agent]; ok && w.sess != nil {
			w.sess.CurrentModeId = cmu.CurrentModeId
		}
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
// doc comment on Model. Also snapshots the raw used/size into
// m.stats for the `stats` command (see agentStats' doc comment) —
// recorded unconditionally, even when size<=0 would make the compaction
// check below meaningless, since a lone `used` figure (or knowing an
// agent hasn't reported any usage shape at all) is still worth showing.
func (m *Model) trackUsage(agent string, used, size int) {
	m.stats.usageUsed[agent] = used
	m.stats.usageSize[agent] = size
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
	m.relayout()
	return *m
}

// relayout recomputes viewport/input dimensions from m.width/m.height and
// the current suggestion popup's line count. Called from handleResize AND
// from handleKey's wrapper (a popup opening/closing mid-typing resizes the
// viewport just like a terminal resize does) so there's one source of
// truth instead of two drifting copies. Must match View()'s layout
// line-for-line: viewport, "\n" separator, two status lines, the
// suggestion overlay (suggestOverlayLineCount()/renderSuggestOverlay() are
// the same computation, so they can't disagree), then the input box (1
// border line + inputHeight). Under-reserving clips the input box off the
// bottom.
func (m *Model) relayout() {
	if !m.ready {
		return
	}
	separatorHeight := 1
	statusHeight := 2
	overlayHeight := m.suggestOverlayLineCount()
	inputBoxHeight := 1 + inputHeight // border line + textarea content lines
	vpHeight := m.height - separatorHeight - statusHeight - overlayHeight - inputBoxHeight
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
}

// handleKey wraps handleKeyDispatch with the bookkeeping that must run
// after EVERY keypress regardless of which branch inside it fired:
// refreshSuggest recomputes the live "/"/"@" popup from whatever the input
// box now contains, and relayout resizes the viewport to match — done
// here, once, rather than scattered across handleKeyDispatch's many early
// returns, so there's exactly one place that can get this wrong instead of
// N of them.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	newModel, cmd := m.handleKeyDispatch(msg)
	mm := newModel.(Model)
	mm.refreshSuggest()
	mm.relayout()
	return mm, cmd
}

// handleKeyDispatch routes a keypress through m.mode()'s priority chain
// (permission answer > routeAsk answer > normal dispatch), exactly the
// original REPL's lineCh-handling order (main.go's old select loop).
func (m Model) handleKeyDispatch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.lastCopyStatus = "" // one-shot — see its doc comment
	if m.selectionActive {
		// Dismiss a lingering selection highlight on the next keypress,
		// same one-shot lifespan as lastCopyStatus above. Only re-syncs
		// the viewport when there was actually something to clear, so an
		// ordinary keypress with no prior selection doesn't pay for a
		// redundant re-render.
		m.selectionActive = false
		m.syncViewport()
	}
	switch msg.Type {
	case tea.KeyCtrlC:
		// First press clears the in-progress draft rather than quitting
		// outright — an immediate, unconfirmed Ctrl+C used to kill every
		// connected agent's subprocess at once. Only quits once the input
		// is already empty, which a second press naturally satisfies
		// without needing a press-twice timer.
		if m.input.Value() != "" {
			m.input.Reset()
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit
	case tea.KeyEsc:
		// Only in modeNormal — a pending permission/routeAsk/contextLevel
		// menu already has its own answer paths (type "cancel", arrow+enter,
		// etc.), and those have real, tested invariants (concurrency
		// invariants #4/#10/#11) not worth disturbing here. Esc's job is
		// strictly "stop a running turn without quitting" — see
		// interruptBusyAgents' doc comment.
		if m.mode() == modeNormal {
			// Dismissing an open "/"/"@" suggestion popup takes priority
			// over interrupting a busy agent — the popup is what's
			// actually in front of the user's attention right now.
			// Suppressed by exact token, not just "hide until next
			// keystroke": continuing to type without changing the active
			// token (e.g. an arrow key that isn't bound to anything here)
			// must keep it dismissed.
			if m.suggestKind != suggestNone {
				m.suggestSuppressed = true
				m.suggestSuppressedToken = m.suggestToken
				m.suggestKind = suggestNone
				m.suggestItems = nil
				return m, nil
			}
			return m.interruptBusyAgents(), nil
		}
	case tea.KeyCtrlV:
		// A literal Ctrl+V keystroke only ever reaches the program at all
		// when the terminal had nothing to bracket-paste as text — i.e.
		// the clipboard holds an image, or nothing. Only meaningful in
		// modeNormal, same as Esc above; otherwise falls through
		// unhandled (a permission/routeAsk answer is text-only).
		if m.mode() == modeNormal {
			return m.handleClipboardImagePaste(), nil
		}
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

	// An open "/"-command or "@"-file suggestion popup claims Up/Down/Tab/
	// Enter next, before the prompt-history recall or normal submit below:
	// arrow keys move the highlighted row, Tab/Enter accepts it. Never
	// intercepts a plain character key — those must still reach the
	// textarea so the filter keeps narrowing.
	if m.mode() == modeNormal && m.suggestKind != suggestNone {
		switch msg.Type {
		case tea.KeyUp:
			m.suggestCursor--
			if m.suggestCursor < 0 {
				m.suggestCursor = len(m.suggestItems) - 1
			}
			return m, nil
		case tea.KeyDown:
			m.suggestCursor++
			if m.suggestCursor >= len(m.suggestItems) {
				m.suggestCursor = 0
			}
			return m, nil
		case tea.KeyTab, tea.KeyEnter:
			return m.acceptSuggestion(), nil
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

	// A large bracketed paste (bubbletea delivers the WHOLE pasted blob as
	// one Key{Type: KeyRunes, Paste: true}, never split into per-character
	// events) collapses to a short "[Pasted text #N +NN lines]" chip
	// instead of ballooning the input box. expandPastes restores the full
	// text at the actual dispatch choke point — nothing is lost. A small
	// paste falls through unchanged to the ordinary textarea handling below.
	if msg.Paste {
		if chip, ok := m.storePasteIfLarge(string(msg.Runes)); ok {
			m.input.InsertString(chip)
			return m, nil
		}
	}

	// A trailing "\" before Enter inserts a newline instead of submitting —
	// a terminal-agnostic multi-line continuation that works even where
	// Shift+Enter isn't decoded as a distinct key (this bubbletea version
	// has no kitty-keyboard-protocol support, so a terminal that encodes
	// Shift+Enter that way is indistinguishable from plain Enter here) and
	// where ctrl+j isn't already muscle memory.
	if msg.Type == tea.KeyEnter {
		if val := m.input.Value(); strings.HasSuffix(val, "\\") {
			m.input.SetValue(strings.TrimSuffix(val, "\\") + "\n")
			return m, nil
		}
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

// handleClipboardImagePaste implements Ctrl+V's "attach an image straight
// from the OS clipboard" — the direct analog of typing `@screenshot.png`
// but without saving the screenshot to a file yourself first. Reuses the
// existing @-attachment pipeline (extractAttachments/buildPromptBlocks)
// rather than a second image-content mechanism: the clipboard bytes are
// saved to a real file under m.imageDir, then referenced as an ordinary
// "@path" token. This handler fires on every Ctrl+V that reaches the
// program (see the KeyCtrlV case in handleKeyDispatch for when that is),
// so a clipboard with no image is silently a no-op; a real read/decode
// failure still gets a line.
func (m Model) handleClipboardImagePaste() Model {
	data, mime, err := readClipboardImage()
	if err != nil {
		if !errors.Is(err, ErrNoClipboardImage) {
			m.appendLine(fmt.Sprintf("clipboard image paste failed: %v\n", err))
			m.syncViewport()
		}
		return m
	}
	path, err := m.saveClipboardImage(data, mime)
	if err != nil {
		m.appendLine(fmt.Sprintf("clipboard image paste failed: %v\n", err))
		m.syncViewport()
		return m
	}
	m.input.InsertString("@" + path + " ")
	return m
}

// saveClipboardImage writes data under m.imageDir (Config.ImageDir's doc
// comment), falling back to os.TempDir() if it's unset/uncreatable rather
// than failing the paste outright. 0o600 (owner-only), matching every
// other chorus-written file under this directory (security invariant #6).
func (m Model) saveClipboardImage(data []byte, mime string) (string, error) {
	dir := m.imageDir
	if dir == "" || os.MkdirAll(dir, 0o700) != nil {
		dir = os.TempDir()
	}
	ext := ".png"
	if mime == "image/jpeg" {
		ext = ".jpg"
	}
	path := filepath.Join(dir, fmt.Sprintf("clipboard-%d%s", time.Now().UnixNano(), ext))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// pasteCollapseCharThreshold/pasteCollapseLineThreshold gate the large-paste
// collapse feature (>800 chars OR >3 lines) — high enough that ordinary
// pasted snippets pass through untouched, low enough that a pasted log or
// diff gets collapsed before it dominates the input box.
const (
	pasteCollapseCharThreshold = 800
	pasteCollapseLineThreshold = 3
)

// storePasteIfLarge records text under a fresh chip key (Model.pastes) and
// returns that chip if text crosses either collapse threshold; ok is false
// (chip is "") for an ordinary small paste, telling the caller to fall
// through to normal insertion instead.
func (m *Model) storePasteIfLarge(text string) (chip string, ok bool) {
	lines := strings.Count(text, "\n") + 1
	if len(text) <= pasteCollapseCharThreshold && lines <= pasteCollapseLineThreshold {
		return "", false
	}
	m.nextPasteID++
	chip = fmt.Sprintf("[Pasted text #%d +%d lines]", m.nextPasteID, lines)
	m.pastes[chip] = text
	return chip, true
}

// expandPastes replaces every chip token storePasteIfLarge previously
// inserted with the full text it stands in for — called once, at the
// actual dispatch choke point (dispatchUserTurn/handleNativeCommand), so
// the input box, scrollback echo, and promptHistory all keep showing the
// short chip form; only the agent or shell ever sees the expanded text.
// Plain string replacement, not a regex: chip keys are already exact,
// unique strings (Model.pastes' doc comment), so there's nothing to parse.
func (m Model) expandPastes(text string) string {
	if len(m.pastes) == 0 || !strings.Contains(text, "[Pasted text #") {
		return text
	}
	for chip, full := range m.pastes {
		text = strings.ReplaceAll(text, chip, full)
	}
	return text
}

// suggestMaxVisible caps how many popup rows relayout ever reserves space
// for — a long unfiltered match list (a bare "@" in a big directory, say)
// still only costs a fixed, small number of lines, with a "N more" line
// standing in for the rest.
const suggestMaxVisible = 6

// detectSuggestToken looks at the CURSOR-TRAILING end of the input value
// only (not wherever the cursor actually is) — a deliberate simplification:
// tracking the exact rune/column the cursor sits at through a wrapped,
// multi-line textarea would be materially more code for a case (editing
// back into the MIDDLE of already-typed text to add a new "/" or "@"
// mention) that's rare in practice, since both kinds of mention are
// almost always typed at the point you're actively composing. Slash
// commands mirror dispatch()'s own rule exactly (the ENTIRE trimmed line
// must start with "/", with no space yet — i.e. still composing the
// command name itself); "@" mentions match the trailing whitespace-
// delimited word anywhere, since a real @-attachment can appear mid-
// sentence.
func detectSuggestToken(val string) (kind suggestKind, tokenStart int, token string) {
	if val == "" {
		return suggestNone, 0, ""
	}
	if strings.HasPrefix(val, "/") && !strings.ContainsAny(val, " \t\n") {
		return suggestCommand, 0, val
	}
	lastWS := strings.LastIndexAny(val, " \t\n")
	word := val[lastWS+1:]
	if strings.HasPrefix(word, "@") {
		return suggestFile, lastWS + 1, word
	}
	return suggestNone, 0, ""
}

// refreshSuggest recomputes the live suggestion popup from the input box's
// current content — called exactly once per keypress, from handleKey's
// wrapper, regardless of which branch inside handleKeyDispatch actually
// ran. Model.suggestKind's doc comment explains why this is cached rather
// than recomputed by View()/relayout() themselves.
func (m *Model) refreshSuggest() {
	kind, tokenStart, token := detectSuggestToken(m.input.Value())
	if kind != suggestNone && m.suggestSuppressed && token == m.suggestSuppressedToken {
		kind = suggestNone
	}
	var items []suggestItem
	if kind == suggestCommand {
		items = matchingCommandSuggestions(m.commands, token[1:])
	} else if kind == suggestFile {
		items = matchingFileSuggestions(m.cwd, token[1:])
	}
	if len(items) == 0 {
		m.suggestKind = suggestNone
		m.suggestItems = nil
		m.suggestTokenStart = 0
		return
	}
	if token != m.suggestToken {
		m.suggestCursor = 0
	}
	if token != m.suggestSuppressedToken {
		m.suggestSuppressed = false
	}
	m.suggestToken = token
	m.suggestKind = kind
	m.suggestItems = items
	m.suggestTokenStart = tokenStart
}

// acceptSuggestion replaces the active token (from suggestTokenStart to
// the end of the input, which is always where the token runs to — see
// detectSuggestToken) with the currently highlighted item's full
// replacement text, then closes the popup implicitly: the very next
// refreshSuggest call (handleKey's wrapper, right after this returns) will
// no longer find a matching token unless the replacement itself opens a
// new one (e.g. accepting a directory keeps "@" popup open one level
// deeper — matchingFileSuggestions' doc comment).
func (m Model) acceptSuggestion() Model {
	if m.suggestKind == suggestNone || len(m.suggestItems) == 0 {
		return m
	}
	idx := m.suggestCursor
	if idx < 0 || idx >= len(m.suggestItems) {
		idx = 0
	}
	val := m.input.Value()
	if m.suggestTokenStart > len(val) {
		return m
	}
	m.input.SetValue(val[:m.suggestTokenStart] + m.suggestItems[idx].insert)
	return m
}

// suggestOverlayLineCount and renderSuggestOverlay are two views of the
// exact same computation (the latter calls the former's sibling logic
// directly) — relayout() sizes the viewport by the count, View() renders
// the string, and they can never disagree because neither recomputes the
// item list itself (both just read Model.suggestItems, already cached by
// refreshSuggest).
func (m Model) suggestOverlayLineCount() int {
	if m.suggestKind == suggestNone || len(m.suggestItems) == 0 {
		return 0
	}
	shown := len(m.suggestItems)
	if shown > suggestMaxVisible {
		shown = suggestMaxVisible
	}
	lines := shown
	if len(m.suggestItems) > shown {
		lines++
	}
	return lines
}

var suggestCursorStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))

// renderSuggestOverlay renders the live popup as plain lines, one per
// matched item (cursor-highlighted row marked "❯", same glyph convention
// as render.FormatMenuLine's permission/routeAsk menus) — never appended
// to m.blocks/the scrollback document: unlike a permission prompt, this
// has no reason to leave any permanent trace once you've moved on, and
// only View() ever calls this (Model.blocks stays untouched), so there's
// nothing to clean up when the popup closes.
func (m Model) renderSuggestOverlay() string {
	n := m.suggestOverlayLineCount()
	if n == 0 {
		return ""
	}
	shown := len(m.suggestItems)
	if shown > suggestMaxVisible {
		shown = suggestMaxVisible
	}
	var b strings.Builder
	for i := 0; i < shown; i++ {
		it := m.suggestItems[i]
		marker := "  "
		if i == m.suggestCursor {
			marker = suggestCursorStyle.Render("❯ ")
		}
		label := render.StripANSI(it.label)
		desc := render.StripANSI(it.desc)
		if desc != "" {
			fmt.Fprintf(&b, "%s%-24s %s\n", marker, label, desc)
		} else {
			fmt.Fprintf(&b, "%s%s\n", marker, label)
		}
	}
	if len(m.suggestItems) > shown {
		fmt.Fprintf(&b, "  … %d more\n", len(m.suggestItems)-shown)
	}
	return b.String()
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
	// A slash command (isPrompt=false) — never handoff-wrapped, but still
	// funneled through the shared choke point for consistent echo/record.
	m.dispatchUserTurn(agent, m.pendingRoute.text, false, true)
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

// handleModeCommand parses and executes "mode <agent> <id-or-name>" —
// an explicit ACP session/set_mode switch. Expects the id/name to have
// been discovered via `modes` first — chorus never guesses one (see
// session.Spec.AutoMode's doc comment). The RPC itself runs
// asynchronously via runSetMode (concurrency invariant #1); this only
// validates the input and kicks it off.
func (m Model) handleModeCommand(rest string) (tea.Model, tea.Cmd) {
	idx := strings.IndexByte(rest, ' ')
	if idx < 0 {
		m.appendLine("usage: mode <agent> <id-or-name> — see `modes` to list what's available\n")
		m.syncViewport()
		return m, nil
	}
	agentTok, modeTok := rest[:idx], strings.TrimSpace(rest[idx+1:])
	agent, ok := matchName(agentTok, agentNames(m.workers))
	if !ok {
		m.appendLine(fmt.Sprintf("unknown agent %q\n", agentTok))
		m.syncViewport()
		return m, nil
	}
	w := m.workers[agent]
	modeId, ok := resolveMode(w, modeTok)
	if !ok {
		m.appendLine(fmt.Sprintf("%s: no mode matching %q — see `modes`\n", agent, modeTok))
		m.syncViewport()
		return m, nil
	}
	return m, runSetMode(m.ctx, w, agent, modeId, modeTok, false, false, "")
}

// handleAutoCommand implements the `auto`/`auto <agent>` toggle: switch
// into (or back out of) whichever ACP session mode agents.yaml's
// auto_mode names for the given agent(s) — chorus's answer to "Claude/
// Gemini/opencode's own auto-accept/yolo mode," without hardcoding what
// that mode is actually called for any of them (session.Spec.AutoMode's
// doc comment). No argument applies to every connected agent that has
// auto_mode configured. Whether an agent is "currently in auto mode" is
// decided by Model.autoPrevMode's presence, not by comparing
// CurrentModeId to AutoMode — the agent could have separately switched
// itself to the exact same mode ID on its own, which shouldn't be
// treated as "chorus put it there, toggle it off." The actual mode
// switch happens asynchronously (runSetMode); autoPrevMode itself is
// only updated once setModeResultMsg confirms the RPC succeeded.
func (m Model) handleAutoCommand(arg string) (tea.Model, tea.Cmd) {
	var targets []string
	if arg == "" {
		for _, spec := range m.agentSpecs {
			if spec.AutoMode == "" {
				continue
			}
			if _, ok := m.workers[spec.Name]; ok {
				targets = append(targets, spec.Name)
			}
		}
		if len(targets) == 0 {
			m.appendLine("no connected agent has auto_mode configured in agents.yaml\n")
			m.syncViewport()
			return m, nil
		}
	} else {
		agent, ok := matchName(arg, agentNames(m.workers))
		if !ok {
			m.appendLine(fmt.Sprintf("unknown agent %q\n", arg))
			m.syncViewport()
			return m, nil
		}
		targets = []string{agent}
	}

	var cmds []tea.Cmd
	for _, agent := range targets {
		w := m.workers[agent]
		spec := specByName(m.agentSpecs, agent)
		if spec.AutoMode == "" {
			m.appendLine(fmt.Sprintf("%s: no auto_mode configured in agents.yaml — use `modes` to find the real value, then set it there\n", agent))
			continue
		}
		if prev, inAuto := m.autoPrevMode[agent]; inAuto {
			cmds = append(cmds, runSetMode(m.ctx, w, agent, prev, string(prev), true, false, ""))
			continue
		}
		modeId, ok := resolveMode(w, spec.AutoMode)
		if !ok {
			m.appendLine(fmt.Sprintf("%s: auto_mode %q doesn't match any mode it advertised — see `modes`\n", agent, spec.AutoMode))
			continue
		}
		cmds = append(cmds, runSetMode(m.ctx, w, agent, modeId, spec.AutoMode, true, true, w.CurrentModeId()))
	}
	m.syncViewport()
	return m, tea.Batch(cmds...)
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
	if line == "modes" {
		m.appendLine(formatModes(m.agentSpecs, m.workers))
		m.syncViewport()
		return m, nil
	}
	if strings.HasPrefix(line, "mode ") {
		return m.handleModeCommand(strings.TrimSpace(strings.TrimPrefix(line, "mode ")))
	}
	if line == "auto" || strings.HasPrefix(line, "auto ") {
		arg := strings.TrimSpace(strings.TrimPrefix(line, "auto"))
		return m.handleAutoCommand(arg)
	}
	if line == "context" {
		m.contextMenuOpen = true
		m.contextCursor = contextLevelIndexOf(m.routing.ContextLevelOrDefault())
		m.blocks = append(m.blocks, block{text: formatContextLevelMenu(m.contextCursor)})
		m.contextMenuIndex = len(m.blocks) - 1
		m.syncViewport()
		return m, nil
	}

	agent, sentText, isPrompt, msg, ask, decisionReq := dispatch(line, m.workers, m.defaultAgent, m.routing, m.commands)
	if msg != "" {
		m.appendLine(msg + "\n")
	}
	if agent != "" {
		m.dispatchUserTurn(agent, sentText, isPrompt, true)
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
//
// Echoes the prompt FIRST, before anything routing-related — found live
// (2026-09, user report): the previous order showed "[routing] asking X
// to decide..." before the user's own prompt ever appeared on screen,
// which read backwards (you'd see chorus reacting to a question you
// hadn't been shown yet). The target agent isn't known until the
// decision resolves, so this echoes under a neutral "routing" tag rather
// than the eventual agent's — dispatchUserTurn's OWN echo is then skipped
// (echo=false) at both of this function's dispatch call sites below, to
// avoid showing the same prompt twice once the real agent is known.
func (m Model) startRouteDecision(text string) (tea.Model, tea.Cmd) {
	m.appendLine(m.renderer.FormatUserPrompt("routing", text))

	conn, ok := m.conns[m.routing.DecisionAgent]
	if !ok {
		// Config error (decision_agent unset, misspelled, or not
		// connected) — don't cost a timeout on a call that can't succeed;
		// fall back to defaultAgent synchronously, same as routing "off".
		m.appendLine(fmt.Sprintf("routing.decision_agent %q isn't connected — falling back to %s\n", m.routing.DecisionAgent, m.defaultAgent))
		m.dispatchUserTurn(m.defaultAgent, text, true, false)
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

	m.dispatchUserTurn(target, msg.prompt, true, false)
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

// dispatchUserTurn queues one resolved user turn to target, optionally
// echoing it, and records it in the activity log exactly like every
// successful dispatch path does. When isPrompt is true (free text, not a
// slash command) and this turn switches away from the agent that handled
// the previous one, it prepends a one-time handoff preamble so the
// newly-selected agent — which runs on its own isolated ACP session with
// no memory of the prior turns — still gets the conversation so far as
// context. This is the single choke point every switch-capable path
// funnels through (explicit "<agent>:" prefix and routing "off" via
// handleNormalLine, the LLM router via handleRouteDecision, its fallback
// via startRouteDecision), so the preamble behaves identically no matter
// how the switch was triggered. Slash commands (isPrompt false) are
// control commands, never preamble-wrapped — prepending conversational
// context to a "/compact" or "/model" would only confuse the command.
//
// echo controls whether this call renders the "echo what was asked" block
// itself. False for both LLM-routing call sites (startRouteDecision's
// fallback, handleRouteDecision's resolved dispatch) — startRouteDecision
// already echoed the prompt under a neutral "routing" tag before the
// decision was even made (see its own doc comment for why), so echoing
// again here under the now-known target agent would show the same prompt
// twice. True everywhere else, where this is the first and only echo.
//
// The echo and activity record always use the raw text, never the
// preamble-wrapped prompt — the preamble is scaffolding for the agent, not
// something to show the user or replay as prior context on the next switch.
func (m *Model) dispatchUserTurn(target, text string, isPrompt, echo bool) {
	// Expanded here, not earlier: text itself (used below for the echo and
	// the activity log) must stay in its short chip form — only the actual
	// wire content sent to the agent needs the real pasted text. See
	// expandPastes' doc comment.
	promptText := m.expandPastes(text)
	if isPrompt && m.lastRoutedAgent != "" && m.lastRoutedAgent != target {
		promptText = buildHandoffPreamble(m.lastRoutedAgent, m.activityContext("full")) + "\n\n" + promptText
	}
	if errMsg := queuePrompt(target, promptText, m.workers); errMsg != "" {
		m.appendLine(errMsg + "\n")
		return
	}
	if echo {
		// Echo what was actually asked — ACP never sends this back on a
		// live turn (only on session/load history replay), so without
		// this the only thing that ever appeared was the reply, with no
		// way to tell which reply answered which question. Found live.
		m.appendLine(m.renderer.FormatUserPrompt(target, text))
	}
	m.recordDispatch(target, text)
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

// These bound Model.activity and how each tier renders it. The design
// separates two jobs the log serves with opposite needs (learned live —
// see Model.activity's doc comment): storing turns faithfully (so a handoff
// can carry real work), vs. keeping the router's per-turn decision prompt
// cheap. Storage is near-verbatim; truncation moved to RENDER, per tier.
//
//   - activityLogCap:        max entries retained (merged-by-turn, so this
//     is turns, not streamed chunks); oldest dropped past it.
//   - activityEntryCap:      per-entry STORAGE cap — high, only to bound a
//     single pathological turn's memory, not to shrink real content (this
//     is what used to be 1000 and silently ate multi-phase plans on
//     handoff — "only Phase 1 survived").
//   - activityDigestN:       entries the cheap "digest" tier renders.
//   - activityDigestEntryCap: per-entry cap applied when RENDERING the
//     digest tier — keeps the router's decision prompt small even though
//     entries are now stored near-verbatim.
//   - activityHandoffBudget: total char budget the "full" tier (what a
//     cross-agent handoff is built from) renders within, oldest entries
//     dropped first — complete enough to continue the work, bounded enough
//     not to blow the receiving agent's context window.
const (
	activityLogCap         = 60
	activityDigestN        = 8
	activityEntryCap       = 16000
	activityDigestEntryCap = 500
	activityHandoffBudget  = 48000
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

// formatActivity renders entries as "[key] text" lines. perEntryCap > 0
// truncates each entry's text at render time (the cheap digest tier uses
// this to keep the router's decision prompt small even though entries are
// now stored near-verbatim); totalBudget > 0 keeps only the most recent
// entries whose combined rendered size fits the budget, oldest dropped
// first (the handoff transcript stays complete for recent work without
// risking the receiving agent's context window). 0 disables either cap.
func formatActivity(entries []activityEntry, perEntryCap, totalBudget int) string {
	if totalBudget > 0 {
		entries = trimToBudget(entries, totalBudget)
	}
	var b strings.Builder
	for _, e := range entries {
		text := e.text
		if perEntryCap > 0 && len(text) > perEntryCap {
			text = text[:perEntryCap] + "..."
		}
		fmt.Fprintf(&b, "[%s] %s\n", e.key, text)
	}
	return b.String()
}

// trimToBudget returns the longest suffix of entries whose combined
// rendered cost fits budget, dropping oldest first. The most recent entry
// is always included even if it alone exceeds the budget — sending a single
// oversized latest turn beats sending nothing on a handoff. The +4 per
// entry approximates the "[] \n" framing formatActivity adds.
func trimToBudget(entries []activityEntry, budget int) []activityEntry {
	if len(entries) == 0 {
		return entries
	}
	total := 0
	start := len(entries)
	for i := len(entries) - 1; i >= 0; i-- {
		cost := len(entries[i].key) + len(entries[i].text) + 4
		if i < len(entries)-1 && total+cost > budget {
			break
		}
		total += cost
		start = i
	}
	return entries[start:]
}

// activityContext renders Model.activity at the given tier — "prompt"
// (nothing), "digest" (the last activityDigestN entries, each truncated to
// activityDigestEntryCap: the cheap, mechanical default that keeps the
// router's per-turn decision prompt small), or "full" (every retained entry
// untruncated, within activityHandoffBudget — what a cross-agent handoff is
// built from, so a switch carries real prior work, not a 1000-char stub).
// Unrecognized levels fall back to "digest".
func (m Model) activityContext(level string) string {
	switch level {
	case "prompt":
		return ""
	case "full":
		return formatActivity(m.activity, 0, activityHandoffBudget)
	default:
		n := activityDigestN
		if n > len(m.activity) {
			n = len(m.activity)
		}
		return formatActivity(m.activity[len(m.activity)-n:], activityDigestEntryCap, 0)
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
	return m, runNativeCommand(m.ctx, m.expandPastes(cmdline))
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
	doc := m.renderDocument()
	if m.selectionActive {
		// Display-only concern, deliberately not part of renderDocument
		// itself: copySelection/extractSelection also call renderDocument
		// to know what text to put on the clipboard, and must always see
		// the plain, unhighlighted document — HighlightLine's padding
		// isn't real content, and copying it in would pollute the
		// clipboard with garbage whitespace (a real bug caught by
		// TestModel_HandleMouse_PressDragReleaseCopiesSelection before
		// this split existed).
		doc = m.applySelectionHighlight(doc)
	}
	m.viewport.SetContent(doc)
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

// applySelectionHighlight re-styles the lines between selectAnchorLine and
// selectCurLine (inclusive, order-independent) with a full-width
// background highlight (renderer.HighlightLine — the same convention used
// for the prompt-echo highlight), so click-drag selection (handleMouse)
// gives real-time visual feedback while dragging instead of silently
// copying to the clipboard on release with nothing shown on screen in the
// meantime — a real gap reported live (2026-09).
//
// Deliberately strips each selected line's own ANSI styling first rather
// than layering the highlight's background on top of it: many lines carry
// embedded SGR resets (glamour markdown, diff coloring, tool-call status
// colors) that would cut the highlight short partway through the line if
// simply prepended/appended around the existing codes, leaving a ragged,
// partially-highlighted look — the exact problem HighlightLine's own
// padding already solves for plain text. Trading the underlying syntax
// color for a solid, unambiguous highlight during selection matches how
// most editors/terminals visually treat a selection anyway.
func (m Model) applySelectionHighlight(doc string) string {
	lo, hi := m.selectAnchorLine, m.selectCurLine
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo < 0 {
		lo = 0
	}
	lines := strings.Split(doc, "\n")
	if hi >= len(lines) {
		hi = len(lines) - 1
	}
	for i := lo; i <= hi && i < len(lines); i++ {
		lines[i] = m.renderer.HighlightLine(render.StripANSI(lines[i]))
	}
	return strings.Join(lines, "\n")
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
