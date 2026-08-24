// Package tui implements chorus's bubbletea-based interactive terminal UI:
// a scrolling output viewport plus a fixed input box, replacing the
// original line-based REPL (ANSI cursor-redraw + bufio.Scanner) that lived
// in main.go. See the implementation plan (chorus-spec.md/CLAUDE.md, and
// the design doc this was built from) for the full architecture rationale.
//
// Model owns the channel bridging (outputCh/permCh/errCh/delegateLogCh),
// the permission/routeAsk state machine, and the viewport/textinput
// wiring. internal/render's Renderer stays the pure "how do I format one
// bus.Update" layer — Model is the new "how do I lay that out on screen
// and drive it from bubbletea's event loop" layer.
package tui

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"

	acp "github.com/coder/acp-go-sdk"

	"chorus/internal/bus"
	"chorus/internal/delegate"
	"chorus/internal/policy"
	"chorus/internal/render"
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
)

func (m Model) mode() inputMode {
	if m.pendingPerm != nil {
		return modePermission
	}
	if m.pendingRoute != nil {
		return modeRouteAsk
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

// Config bundles everything Model needs at construction — see main.go's
// phase 3, which builds all of this before ever constructing a Model
// (agent connect, delegate hub, session create/resume all stay exactly as
// they were; only what used to be the select-loop REPL is replaced).
type Config struct {
	Ctx context.Context

	Renderer   *render.Renderer
	Collectors *delegate.Collectors

	Workers    map[string]*AgentWorker
	Routing    policy.Routing
	AgentSpecs []session.Spec
	Conns      map[string]*session.Connection

	OutputCh      chan bus.Update
	PermCh        chan bus.PermissionRequest
	ErrCh         chan ErrMsg
	DelegateLogCh chan delegate.LogEntry
}

// Model is chorus's bubbletea Model — see the package doc.
type Model struct {
	ctx context.Context

	renderer *render.Renderer
	coll     *delegate.Collectors

	workers    map[string]*AgentWorker
	routing    policy.Routing
	agentSpecs []session.Spec
	conns      map[string]*session.Connection
	commands   map[string][]acp.AvailableCommand

	outputCh      chan bus.Update
	permCh        chan bus.PermissionRequest
	errCh         chan ErrMsg
	delegateLogCh chan delegate.LogEntry

	viewport viewport.Model
	input    textinput.Model

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

// New constructs the initial Model. Call tea.NewProgram(New(cfg), ...) —
// see main.go.
func New(cfg Config) Model {
	ti := textinput.New()
	ti.Placeholder = `"<agent>: text", a bare prompt to auto-route, or "/name ..." — try "commands"`
	ti.Focus()
	ti.CharLimit = 0

	// MouseWheelEnabled defaults true on viewport.Model, but no tea.MouseMsg
	// ever arrives to act on it unless main.go's tea.NewProgram opts into
	// tea.WithMouseCellMotion() — deliberately not enabled (see main.go):
	// mouse capture stops the terminal from treating click-drag as native
	// text selection. PgUp/PgDn/Home/End (handleKey, below) cover scrolling
	// instead. The tea.MouseMsg case in Update() is inert dead code until/
	// unless that trade-off is revisited, kept rather than removed so
	// re-enabling mouse support later is a one-line change in main.go.
	vp := viewport.New(0, 0)

	return Model{
		ctx:            cfg.Ctx,
		renderer:       cfg.Renderer,
		coll:           cfg.Collectors,
		workers:        cfg.Workers,
		routing:        cfg.Routing,
		agentSpecs:     cfg.AgentSpecs,
		conns:          cfg.Conns,
		commands:       make(map[string][]acp.AvailableCommand),
		outputCh:       cfg.OutputCh,
		permCh:         cfg.PermCh,
		errCh:          cfg.ErrCh,
		delegateLogCh:  cfg.DelegateLogCh,
		viewport:       vp,
		input:          ti,
		permMenuIndex:  -1,
		routeMenuIndex: -1,
	}
}

// --- messages ---------------------------------------------------------

type outputMsg struct{ u bus.Update }
type outputClosedMsg struct{}
type permissionMsg struct{ req bus.PermissionRequest }
type errChMsg struct{ e ErrMsg }
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
		waitForDelegateLog(m.delegateLogCh),
		waitForCtxDone(m.ctx),
		spinnerTick(),
		textinput.Blink,
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

	case delegateLogMsg:
		m.appendLine(formatDelegateLog(msg.e))
		m.syncViewport()
		return m, waitForDelegateLog(m.delegateLogCh)

	case spinnerTickMsg:
		// Only while nothing else owns the input — matches the original
		// ticker case's "never disturb an active permission/routeAsk
		// prompt" gating.
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
	var status string
	switch m.mode() {
	case modePermission:
		status = statusStyle.Render("awaiting permission answer (↑/↓ + enter, number, name, or \"cancel\")")
	case modeRouteAsk:
		status = statusStyle.Render("awaiting agent choice (↑/↓ + enter, number, or name)")
	}
	if status != "" {
		status += "\n"
	}
	return m.viewport.View() + "\n" + status + inputBoxStyle.Width(m.width).Render(m.input.View())
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
	} else if m.coll.IsRegistered(sid) {
		return m, waitForOutput(m.outputCh)
	}

	if acu := u.Notification.Update.AvailableCommandsUpdate; acu != nil {
		m.commands[u.Agent] = acu.AvailableCommands
	}

	if key, text, ok := m.renderer.FormatUpdate(u); ok {
		m.mergeBlock(key, text)
		m.syncViewport()
	}

	return m, waitForOutput(m.outputCh)
}

func (m *Model) handleResize(msg tea.WindowSizeMsg) Model {
	m.width, m.height = msg.Width, msg.Height
	m.ready = true

	// Must match View()'s exact layout line-for-line: viewport, then a
	// literal "\n" separator, then the (always reserved, even when blank
	// this frame) status line, then inputBoxStyle's rendered output —
	// which is itself 2 lines (its top border, then the input content),
	// not 1. Under-reserving here would make the input box (or its
	// border) get clipped off the bottom by the terminal itself.
	separatorHeight := 1
	statusHeight := 1
	inputBoxHeight := 2 // border line + content line
	vpHeight := m.height - separatorHeight - statusHeight - inputBoxHeight
	if vpHeight < 1 {
		vpHeight = 1
	}
	m.viewport.Width = m.width
	m.viewport.Height = vpHeight
	m.input.Width = m.width - 2 // leave room for the cursor near the edge

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
	// translating a "special key" escape sequence the way pgup/pgdown/
	// home/end do, which is suspected — not yet confirmed, no way to
	// test it from this environment — as the cause of pgup/pgdown not
	// scrolling at all on at least one real Windows console
	// (chorus-spec.md §0). home/end have no default viewport binding, so
	// they're wired directly to GotoTop/GotoBottom instead.
	switch msg.String() {
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	case "home":
		m.viewport.GotoTop()
		return m, nil
	case "end":
		m.viewport.GotoBottom()
		return m, nil
	}

	// Arrow-key menu navigation, only while a permission or routeAsk
	// menu is actually up — otherwise up/down fall through to the input
	// box as normal (textinput has no history feature bound to them, so
	// this doesn't take anything away from ordinary typing). Typing a
	// number/name still works exactly as before regardless of whether
	// the user has also been arrowing around — see handlePermissionAnswer/
	// handleRouteAnswer's empty-line-means-arrow-selection fallback.
	if mode := m.mode(); mode != modeNormal {
		switch msg.Type {
		case tea.KeyUp:
			return m.moveMenuCursor(mode, -1), nil
		case tea.KeyDown:
			return m.moveMenuCursor(mode, 1), nil
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
	default:
		return m.handleNormalLine(line)
	}
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
	}
	m.pendingRoute = nil
	m.routeMenuIndex = -1
	m.syncViewport()
	return m, nil
}

func (m Model) handleNormalLine(line string) (tea.Model, tea.Cmd) {
	if line == "" {
		return m, nil
	}
	if line == "quit" || line == "exit" {
		m.quitting = true
		return m, tea.Quit
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

	msg, ask := dispatch(line, m.workers, m.routing, m.commands)
	if msg != "" {
		m.appendLine(msg + "\n")
	}
	if ask != nil {
		m.pendingRoute = ask
		m.routeCursor = 0
		m.blocks = append(m.blocks, block{text: formatRouteAskPrompt(ask, m.workers, m.routeCursor)})
		m.routeMenuIndex = len(m.blocks) - 1
	}
	m.syncViewport()
	return m, nil
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
func (m *Model) syncViewport() {
	atBottom := m.viewport.AtBottom()
	m.viewport.SetContent(m.renderDocument())
	if atBottom {
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
