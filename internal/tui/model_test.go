package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func newTestModel(t *testing.T, workers map[string]*AgentWorker, routing policy.Routing) Model {
	t.Helper()
	m := New(Config{
		Ctx:           context.Background(),
		Renderer:      render.New(),
		Collectors:    delegate.NewCollectors(),
		Workers:       workers,
		Routing:       routing,
		OutputCh:      make(chan bus.Update, 64),
		PermCh:        make(chan bus.PermissionRequest, 4),
		ErrCh:         make(chan ErrMsg, 8),
		DoneCh:        make(chan PromptDoneMsg, 8),
		DelegateLogCh: make(chan delegate.LogEntry, 8),
	})
	// Simulate the initial WindowSizeMsg bubbletea sends on startup, so
	// the viewport/input are sized and syncViewport has somewhere to
	// write — without this, m.ready stays false and View() short-circuits.
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return updated.(Model)
}

func newWorker() *AgentWorker {
	return &AgentWorker{in: make(chan []acp.ContentBlock, 16)}
}

// enterWithInput sets the input box's value directly, then runs it
// through the exact same real Enter-key path (Model.Update's
// tea.KeyMsg{Type: tea.KeyEnter} case, which reads m.input.Value()) that a
// user typing text and pressing Enter would go through — skips simulating
// individual keystrokes (which would exercise textinput's own tested
// behavior, not chorus's) without adding any test-only code to Model
// itself.
func enterWithInput(m Model, text string) (Model, tea.Cmd) {
	m.input.SetValue(text)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return updated.(Model), cmd
}

// --- (1) permission interrupts a routeAsk without discarding it --------

func TestModel_PermissionInterruptsRouteAskAndResumes(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker(), "opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	// A slash command both connected agents advertise is ambiguous — the
	// only remaining source of a routeAsk now that keyword-based "ask
	// when ambiguous" auto-routing has been removed entirely.
	m.commands = map[string][]acp.AvailableCommand{
		"claude":   {{Name: "plan"}},
		"opencode": {{Name: "plan"}},
	}

	m, _ = enterWithInput(m, "/plan some ambiguous prompt")
	if m.pendingRoute == nil {
		t.Fatal("pendingRoute = nil, want a pending route after an ambiguous slash command")
	}
	if m.mode() != modeRouteAsk {
		t.Fatalf("mode() = %v, want modeRouteAsk", m.mode())
	}
	savedRoute := m.pendingRoute

	// A permission request arrives mid-routeAsk — must take display
	// priority WITHOUT discarding the outstanding routeAsk.
	resp := make(chan acp.RequestPermissionResponse, 1)
	req := bus.PermissionRequest{
		Agent: "claude",
		Req: acp.RequestPermissionRequest{
			Options: []acp.PermissionOption{{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce}},
		},
		Resp: resp,
	}
	updated, _ := m.Update(permissionMsg{req})
	m = updated.(Model)

	if m.mode() != modePermission {
		t.Fatalf("mode() = %v, want modePermission once a permission request arrives", m.mode())
	}
	if m.pendingRoute != savedRoute {
		t.Fatal("pendingRoute was discarded by the interrupting permission request — it must survive until answered")
	}

	// Answer the permission.
	m, _ = enterWithInput(m, "allow")
	select {
	case out := <-resp:
		if out.Outcome.Selected == nil || out.Outcome.Selected.OptionId != "allow" {
			t.Fatalf("permission outcome = %+v, want Selected allow", out.Outcome)
		}
	default:
		t.Fatal("permission answer was never sent on Resp")
	}
	if m.pendingPerm != nil {
		t.Fatal("pendingPerm still set after being answered")
	}

	// The routeAsk must resume — mode() falls back to modeRouteAsk, not
	// modeNormal, since it was never resolved.
	if m.mode() != modeRouteAsk {
		t.Fatalf("mode() = %v after answering the permission, want modeRouteAsk (the interrupted routeAsk should resume)", m.mode())
	}
	if m.pendingRoute == nil {
		t.Fatal("pendingRoute = nil after resuming — it should still be the original ambiguous prompt")
	}

	// Finally resolve the routeAsk itself.
	m, _ = enterWithInput(m, "claude")
	if m.pendingRoute != nil {
		t.Fatal("pendingRoute still set after answering it")
	}
	select {
	case blocks := <-workers["claude"].in:
		if len(blocks) != 1 || blocks[0].Text == nil || blocks[0].Text.Text != "/plan some ambiguous prompt" {
			t.Fatalf("claude's queued blocks = %+v, want the original slash-command line", blocks)
		}
	default:
		t.Fatal("claude never received the queued prompt after the routeAsk resumed and was answered")
	}
}

// --- (2) FIFO ordering of queued permission requests --------------------

func TestModel_PermissionRequestsQueueFIFOWhileOneIsPending(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	resp1 := make(chan acp.RequestPermissionResponse, 1)
	req1 := bus.PermissionRequest{Agent: "claude", Req: acp.RequestPermissionRequest{
		Options: []acp.PermissionOption{{OptionId: "a1", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce}},
	}, Resp: resp1}

	updated, cmd := m.Update(permissionMsg{req1})
	m = updated.(Model)
	if cmd != nil {
		t.Fatal("Update(permissionMsg) returned a re-arm Cmd — waitForPermission must NOT be re-armed while a permission is already pending, or a second request could jump the first's answer")
	}

	// A second request arrives while the first is still unanswered — in
	// the real program this would be sent on permCh directly (nothing is
	// reading it right now, matching the original nil-channel-toggle
	// behavior), so simulate that by writing to the channel itself.
	resp2 := make(chan acp.RequestPermissionResponse, 1)
	req2 := bus.PermissionRequest{Agent: "opencode", Req: acp.RequestPermissionRequest{
		Options: []acp.PermissionOption{{OptionId: "a2", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce}},
	}, Resp: resp2}
	m.permCh <- req2

	// Answer the first request.
	m, cmd = enterWithInput(m, "allow")
	select {
	case out := <-resp1:
		if out.Outcome.Selected == nil || out.Outcome.Selected.OptionId != "a1" {
			t.Fatalf("req1 outcome = %+v, want Selected a1", out.Outcome)
		}
	default:
		t.Fatal("req1 was never answered")
	}
	if cmd == nil {
		t.Fatal("answering the pending permission must re-arm waitForPermission (cmd == nil), or a queued second request would never be picked up")
	}

	// The re-armed Cmd must pick up req2 — the one that queued while req1
	// was pending — proving FIFO order rather than, say, being dropped or
	// requiring another external send to be noticed.
	msg := cmd()
	pm, ok := msg.(permissionMsg)
	if !ok {
		t.Fatalf("re-armed Cmd produced %#v, want a permissionMsg", msg)
	}
	if pm.req.Agent != "opencode" {
		t.Fatalf("re-armed Cmd delivered %+v, want the queued req2 (agent=opencode)", pm.req)
	}
}

// --- (3) delegation sub-session output never reaches blocks -------------

func TestModel_CollectorRegisteredSessionOutputNeverReachesBlocks(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.coll.Register("sub-session-1")

	before := len(m.blocks)

	// A text-bearing update on the registered sub-session.
	updated, _ := m.Update(outputMsg{u: bus.Update{
		Agent: "claude",
		Notification: acp.SessionNotification{
			SessionId: "sub-session-1",
			Update: acp.SessionUpdate{
				AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("sub-session reply")},
			},
		},
	}})
	m = updated.(Model)
	if len(m.blocks) != before {
		t.Fatalf("blocks grew from %d to %d after a registered sub-session's text update — it must be collected, not rendered", before, len(m.blocks))
	}
	if got := m.coll.Collect("sub-session-1"); got != "sub-session reply" {
		t.Fatalf("Collect() = %q, want the sub-session's reply text to have been accumulated", got)
	}

	m.coll.Register("sub-session-2")
	// A non-text update (tool call) on a registered sub-session — must
	// also be hidden via the IsRegistered fallback, not just text chunks.
	updated, _ = m.Update(outputMsg{u: bus.Update{
		Agent: "claude",
		Notification: acp.SessionNotification{
			SessionId: "sub-session-2",
			Update: acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tc1", Title: "Read x", Status: acp.ToolCallStatusCompleted},
			},
		},
	}})
	m = updated.(Model)
	if len(m.blocks) != before {
		t.Fatalf("blocks grew after a registered sub-session's tool-call update — non-text updates on a collected session must be hidden too")
	}

	// Contrast case: an ordinary (non-registered) session's update must
	// still render normally.
	updated, _ = m.Update(outputMsg{u: bus.Update{
		Agent: "claude",
		Notification: acp.SessionNotification{
			SessionId: "main-session",
			Update: acp.SessionUpdate{
				AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("visible reply")},
			},
		},
	}})
	m = updated.(Model)
	if len(m.blocks) == before {
		t.Fatal("an ordinary (non-collected) session's update never reached blocks — collector hiding is over-broad")
	}
}

// --- viewport must actually reflect every appended block immediately ---
//
// m.blocks growing isn't enough on its own: the viewport only shows what
// was last handed to SetContent (syncViewport), so a block appended
// without a following syncViewport call is invisible on screen until some
// unrelated later event happens to resync it. Found by inspection, not a
// prior report: permissionMsg/errChMsg/delegateLogMsg's handlers were
// appending a block and returning without calling syncViewport — every
// other block-mutating path does.

func TestModel_PermissionPromptAppearsInViewportImmediately(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	resp := make(chan acp.RequestPermissionResponse, 1)
	updated, _ := m.Update(permissionMsg{bus.PermissionRequest{
		Agent: "claude",
		Req: acp.RequestPermissionRequest{
			ToolCall: acp.ToolCallUpdate{Title: strPtrTUI("Write auth.py")},
			Options:  []acp.PermissionOption{{OptionId: "a", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce}},
		},
		Resp: resp,
	}})
	m = updated.(Model)

	if !strings.Contains(m.viewport.View(), "Write auth.py") {
		t.Fatalf("viewport.View() = %q, want the permission prompt visible immediately, without waiting for an unrelated later event to resync it", m.viewport.View())
	}
}

func TestModel_ErrMsgAppearsInViewportImmediately(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	updated, _ := m.Update(errChMsg{ErrMsg{Agent: "claude", Err: errFixture("connection reset")}})
	m = updated.(Model)

	if !strings.Contains(m.viewport.View(), "connection reset") {
		t.Fatalf("viewport.View() = %q, want the error visible immediately", m.viewport.View())
	}
}

func TestModel_DelegateLogAppearsInViewportImmediately(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	updated, _ := m.Update(delegateLogMsg{delegate.LogEntry{Source: "claude", Target: "opencode", Task: "summarize x"}})
	m = updated.(Model)

	if !strings.Contains(m.viewport.View(), "summarize x") {
		t.Fatalf("viewport.View() = %q, want the delegate log line visible immediately", m.viewport.View())
	}
}

func strPtrTUI(s string) *string { return &s }

type errFixture string

func (e errFixture) Error() string { return string(e) }

// --- arrow-key menu navigation ------------------------------------------

func TestModel_ArrowKeysSelectPermissionOption(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	resp := make(chan acp.RequestPermissionResponse, 1)
	updated, _ := m.Update(permissionMsg{bus.PermissionRequest{
		Agent: "claude",
		Req: acp.RequestPermissionRequest{
			Options: []acp.PermissionOption{
				{OptionId: "deny", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
				{OptionId: "allow", Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
			},
		},
		Resp: resp,
	}})
	m = updated.(Model)
	if m.permCursor != 0 {
		t.Fatalf("permCursor = %d, want 0 initially", m.permCursor)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.permCursor != 1 {
		t.Fatalf("permCursor = %d after one Down, want 1", m.permCursor)
	}
	if !strings.Contains(m.viewport.View(), "❯") {
		t.Fatal("viewport doesn't show a cursor marker after moving it")
	}

	// Enter with no typed text confirms the arrow-selected option (index 1
	// -> "allow"), not option 0.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	select {
	case out := <-resp:
		if out.Outcome.Selected == nil || out.Outcome.Selected.OptionId != "allow" {
			t.Fatalf("outcome = %+v, want the arrow-selected \"allow\" option", out.Outcome)
		}
	default:
		t.Fatal("no permission answer was sent")
	}
}

func TestModel_ArrowCursorWrapsAtBoundaries(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	updated, _ := m.Update(permissionMsg{bus.PermissionRequest{
		Agent: "claude",
		Req: acp.RequestPermissionRequest{
			Options: []acp.PermissionOption{
				{OptionId: "a", Name: "A", Kind: acp.PermissionOptionKindRejectOnce},
				{OptionId: "b", Name: "B", Kind: acp.PermissionOptionKindAllowOnce},
			},
		},
		Resp: make(chan acp.RequestPermissionResponse, 1),
	}})
	m = updated.(Model)

	// Up from index 0 should wrap to the last option (index 1), not go
	// negative or get stuck.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = updated.(Model)
	if m.permCursor != 1 {
		t.Fatalf("permCursor = %d after Up from 0, want 1 (wrapped)", m.permCursor)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.permCursor != 0 {
		t.Fatalf("permCursor = %d after Down from 1, want 0 (wrapped)", m.permCursor)
	}
}

// TestModel_PermissionMenuForcesScrollIntoViewEvenWhenScrolledUp
// reproduces a live user report: a permission menu that appears (or is
// re-rendered by an arrow-key press) while the viewport is scrolled away
// from the bottom rendered only its first line or two on screen — the
// user could see option 1 but not option 2+, and since arrow-key up/down
// are captured for menu-cursor movement rather than viewport scrolling
// while a menu is pending, there was no way to scroll the rest into view.
// syncViewport's old "only GotoBottom if already atBottom" rule is right
// for ordinary streamed output (don't yank a reading user back down) but
// wrong for a blocking menu that needs an answer before anything else can
// proceed.
func TestModel_PermissionMenuForcesScrollIntoViewEvenWhenScrolledUp(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	// Fill the document with enough lines to make the viewport's height
	// (20 lines, given the 80x24 window newTestModel sizes it to) matter,
	// then scroll away from the bottom exactly like a user reading back
	// through earlier output would.
	for i := 0; i < 60; i++ {
		m.appendLine(strings.Repeat("x", 10) + "\n")
	}
	m.syncViewport()
	m.viewport.GotoTop()
	if m.viewport.AtBottom() {
		t.Fatal("test setup: viewport should not be at bottom after GotoTop")
	}

	updated, _ := m.Update(permissionMsg{bus.PermissionRequest{
		Agent: "claude",
		Req: acp.RequestPermissionRequest{
			ToolCall: acp.ToolCallUpdate{Title: strPtrTUI("Write auth.py")},
			Options: []acp.PermissionOption{
				{OptionId: "deny", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
				{OptionId: "allow", Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
			},
		},
		Resp: make(chan acp.RequestPermissionResponse, 1),
	}})
	m = updated.(Model)

	if !m.viewport.AtBottom() {
		t.Fatal("viewport did not scroll to bottom when a permission menu appeared while scrolled up")
	}
	view := m.viewport.View()
	if !strings.Contains(view, "Deny") || !strings.Contains(view, "Allow Once") {
		t.Fatalf("viewport.View() = %q, want both permission options visible, not just the first", view)
	}

	// An arrow-key press while the menu is up must keep it fully visible
	// too, even if something scrolled the view away in between.
	m.viewport.GotoTop()
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if !m.viewport.AtBottom() {
		t.Fatal("viewport did not re-scroll to bottom after an arrow-key menu move")
	}
}

func TestModel_ArrowKeyUpdatesMenuInPlaceDespiteInterleavedOutput(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker(), "opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	updated, _ := m.Update(permissionMsg{bus.PermissionRequest{
		Agent: "claude",
		Req: acp.RequestPermissionRequest{
			Options: []acp.PermissionOption{
				{OptionId: "a", Name: "A", Kind: acp.PermissionOptionKindRejectOnce},
				{OptionId: "b", Name: "B", Kind: acp.PermissionOptionKindAllowOnce},
			},
		},
		Resp: make(chan acp.RequestPermissionResponse, 1),
	}})
	m = updated.(Model)
	menuBlocksBefore := len(m.blocks)

	// A completely unrelated agent's output streams in while the
	// permission is still pending — this used to be exactly the scenario
	// that would break mergeBlock's "only the last block" rule, since the
	// menu is no longer last afterward.
	updated, _ = m.Update(outputMsg{u: bus.Update{
		Agent: "opencode",
		Notification: acp.SessionNotification{
			SessionId: "s2",
			Update: acp.SessionUpdate{
				AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("unrelated update")},
			},
		},
	}})
	m = updated.(Model)
	if len(m.blocks) != menuBlocksBefore+1 {
		t.Fatalf("len(blocks) = %d, want exactly one new block appended for the unrelated output", len(m.blocks))
	}

	// Now move the cursor — it must update the ORIGINAL menu block (still
	// tracked by permMenuIndex), not spam a duplicate menu at the end.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if len(m.blocks) != menuBlocksBefore+1 {
		t.Fatalf("len(blocks) = %d after an arrow keypress, want unchanged (%d) — the menu must update in place, not append a new block", len(m.blocks), menuBlocksBefore+1)
	}
	if !strings.Contains(m.blocks[m.permMenuIndex].text, "❯") {
		t.Fatal("the tracked menu block wasn't updated with the new cursor position")
	}
}

func TestModel_ArrowKeysSelectRouteAskCandidate(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker(), "opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.commands = map[string][]acp.AvailableCommand{
		"claude":   {{Name: "plan"}},
		"opencode": {{Name: "plan"}},
	}

	m, _ = enterWithInput(m, "/plan some ambiguous prompt")
	if m.pendingRoute == nil {
		t.Fatal("pendingRoute = nil, want a pending route")
	}

	names := agentNames(workers) // sorted: [claude, opencode]
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.routeCursor != 1 {
		t.Fatalf("routeCursor = %d, want 1", m.routeCursor)
	}

	m, _ = enterWithInput(m, "")
	select {
	case blocks := <-workers[names[1]].in:
		if len(blocks) != 1 || blocks[0].Text == nil {
			t.Fatalf("queued blocks = %+v, unexpected shape", blocks)
		}
	default:
		t.Fatalf("%s never received the queued prompt after arrow-selecting it", names[1])
	}
}

// --- prompt echo ---------------------------------------------------------
//
// ACP never sends a submitted prompt back to the client on a live turn
// (UserMessageChunk only arrives during session/load history replay), so
// without an explicit echo, nothing records what was actually asked —
// found live: several turns into a real session, there was no way to
// tell which agent reply answered which question.

func TestModel_ExplicitAgentPromptIsEchoed(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	m, _ = enterWithInput(m, "claude: fix the login bug")

	if !strings.Contains(m.viewport.View(), "fix the login bug") {
		t.Fatalf("viewport.View() = %q, want the submitted prompt echoed", m.viewport.View())
	}
	if strings.Contains(m.viewport.View(), "claude: fix the login bug") {
		t.Fatal("echoed text still contains the \"claude: \" prefix — should echo just the text sent, not the raw line")
	}
}

func TestModel_AutoRoutedPromptIsEchoed(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.defaultAgent = "claude"

	m, _ = enterWithInput(m, "summarize the changelog")

	out := m.viewport.View()
	if !strings.Contains(out, "summarize the changelog") {
		t.Fatalf("viewport.View() = %q, want the submitted prompt echoed", out)
	}
}

func TestModel_RouteAskResolvedPromptIsEchoed(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker(), "opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.commands = map[string][]acp.AvailableCommand{
		"claude":   {{Name: "plan"}},
		"opencode": {{Name: "plan"}},
	}

	m, _ = enterWithInput(m, "/plan do the thing")
	if m.pendingRoute == nil {
		t.Fatal("pendingRoute = nil, want a pending route")
	}
	m, _ = enterWithInput(m, "claude")

	if !strings.Contains(m.viewport.View(), "do the thing") {
		t.Fatalf("viewport.View() = %q, want the original prompt echoed once the routeAsk resolves", m.viewport.View())
	}
}

func TestModel_FailedDispatchIsNotEchoed(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	m, _ = enterWithInput(m, "gemini: do something")

	out := m.viewport.View()
	if strings.Contains(out, "do something") {
		t.Fatalf("viewport.View() = %q, want nothing echoed for a prompt that was never actually sent (unknown agent)", out)
	}
	if !strings.Contains(out, "unknown agent") {
		t.Fatalf("viewport.View() = %q, want the unknown-agent error shown", out)
	}
}

// TestModel_EchoedPromptHighlightSurvivesWordWrap guards the real risk in
// render.FormatUserPrompt's full-width background padding: renderDocument
// re-wraps the WHOLE document through wordwrap.String (needed so
// bubbles/viewport doesn't truncate un-wrapped blocks — see
// renderDocument's doc comment). If FormatUserPrompt's padding were ever
// off by even one visible column, the padded line would exceed the
// viewport's width and wordwrap would break it across two lines,
// splitting the highlight's background-color escape from its reset and
// bleeding the highlight color into whatever follows. This only exercises
// through the real Model pipeline (Update -> syncViewport -> wordwrap),
// not FormatUserPrompt in isolation, since that's exactly where the two
// pieces could interact badly without either one, alone, revealing it.
func TestModel_EchoedPromptHighlightSurvivesWordWrap(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	// newTestModel already sends a WindowSizeMsg{80, 24}, sizing the
	// renderer/viewport to 80 — matches what FormatUserPrompt pads to.

	m, _ = enterWithInput(m, "claude: a reasonably ordinary length prompt")

	out := m.viewport.View()
	if !strings.Contains(out, "\x1b[100m") {
		t.Fatalf("viewport.View() = %q, want the highlight escape intact after the wordwrap pass", out)
	}
	// If the highlighted line got split, the color would still be "on"
	// (no intervening reset) when the NEXT block's own text starts —
	// look for the reset appearing before the input box's border, i.e.
	// somewhere before the end of the visible output, not missing/pushed
	// past unrelated content.
	hIdx := strings.Index(out, "\x1b[100m")
	rIdx := strings.Index(out[hIdx:], "\x1b[0m")
	if rIdx == -1 {
		t.Fatal("no reset (\\x1b[0m) found after the highlight started — the color would bleed into everything that follows")
	}
}

// --- native ("!") command execution --------------------------------------

func TestModel_NativeCommandEchoesImmediatelyAndReturnsCmd(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{}, policy.Routing{})

	m, cmd := enterWithInput(m, "!echo hello-native")
	if cmd == nil {
		t.Fatal("handleNativeCommand returned a nil tea.Cmd — the command must run asynchronously, not block Update()")
	}
	out := m.viewport.View()
	if !strings.Contains(out, "echo hello-native") {
		t.Fatalf("viewport.View() = %q, want the command line echoed immediately, before it finishes", out)
	}
	if !strings.Contains(out, "[!]") {
		t.Fatalf("viewport.View() = %q, want the native-command tag present", out)
	}
}

func TestModel_NativeCommandEmptyShowsUsageAndDoesNotRun(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{}, policy.Routing{})

	m, cmd := enterWithInput(m, "!   ")
	if cmd != nil {
		t.Fatal("an empty ! command should not launch a tea.Cmd")
	}
	out := m.viewport.View()
	if !strings.Contains(out, "usage:") {
		t.Fatalf("viewport.View() = %q, want a usage message for an empty ! command", out)
	}
}

// TestModel_NativeCommandResultRendersOutputOnCompletion runs the real
// tea.Cmd handleNativeCommand returns (a real subprocess, not a mock) and
// feeds its result back through Update — end-to-end confirmation that a
// "!" command never touches any agent (workers is empty) and its output
// still renders correctly once complete.
func TestModel_NativeCommandResultRendersOutputOnCompletion(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{}, policy.Routing{})

	_, cmd := enterWithInput(m, "!echo hello-native")
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd from a real ! command")
	}
	msg := cmd()
	result, ok := msg.(nativeCmdResultMsg)
	if !ok {
		t.Fatalf("tea.Cmd produced %T, want nativeCmdResultMsg", msg)
	}
	if result.err != nil {
		t.Fatalf("native command failed: %v (output: %q)", result.err, result.output)
	}

	updated, _ := m.Update(result)
	m = updated.(Model)

	out := m.viewport.View()
	if !strings.Contains(out, "hello-native") {
		t.Fatalf("viewport.View() = %q, want the command's real output rendered", out)
	}
	if !strings.Contains(out, "done in") {
		t.Fatalf("viewport.View() = %q, want a completion status line", out)
	}
}

// --- stats tallying (CLAUDE.md's delegation-maximization plan item 4) --

func TestModel_HandleOutput_TalliesDirectToolCallButNotDelegateCall(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	updated, _ := m.Update(outputMsg{u: bus.Update{
		Agent: "claude",
		Notification: acp.SessionNotification{
			SessionId: "main-session",
			Update: acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tc1", Title: "Edit foo.go", Status: acp.ToolCallStatusCompleted},
			},
		},
	}})
	m = updated.(Model)

	updated, _ = m.Update(outputMsg{u: bus.Update{
		Agent: "claude",
		Notification: acp.SessionNotification{
			SessionId: "main-session",
			Update: acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tc2", Title: delegate.ToolTitlePrefix + "delegate", Status: acp.ToolCallStatusCompleted},
			},
		},
	}})
	m = updated.(Model)

	if got := m.stats.direct["claude"]; got != 1 {
		t.Fatalf("stats.direct[claude] = %d, want 1 (only the non-delegate ToolCall should count)", got)
	}
}

func TestModel_DelegateLogMsg_TalliesSentReceivedAndFailed(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	updated, _ := m.Update(delegateLogMsg{e: delegate.LogEntry{Source: "claude", Target: "opencode", Task: "do X"}})
	m = updated.(Model)
	updated, _ = m.Update(delegateLogMsg{e: delegate.LogEntry{Source: "claude", Target: "opencode", Task: "do Y", Err: context.DeadlineExceeded}})
	m = updated.(Model)

	if got := m.stats.delegateSent["claude"]; got != 2 {
		t.Fatalf("stats.delegateSent[claude] = %d, want 2", got)
	}
	if got := m.stats.delegateRecv["opencode"]; got != 2 {
		t.Fatalf("stats.delegateRecv[opencode] = %d, want 2", got)
	}
	if got := m.stats.delegateFail["claude"]; got != 1 {
		t.Fatalf("stats.delegateFail[claude] = %d, want 1 (only the errored call)", got)
	}
}

func TestModel_StatsCommand_RendersFormattedStats(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.agentSpecs = []session.Spec{{Name: "claude"}}
	m.stats.direct["claude"] = 3

	m, _ = enterWithInput(m, "stats")

	out := m.viewport.View()
	if !strings.Contains(out, "claude") || !strings.Contains(out, "3") {
		t.Fatalf("viewport.View() = %q, want the stats command's output rendered", out)
	}
}

// --- multi-line input: wrapping, ctrl+j, home/end, history --------------
//
// These cover the textinput->textarea swap (chorus-spec.md's same-day
// entry): wrapping is bubbles/textarea's own job (not independently unit
// tested here — there's nothing chorus-specific to verify beyond "we
// configured it," and SetWidth is exercised by handleResize already), but
// ctrl+j-for-newline, home/end/ctrl+a/ctrl+e line editing, and prompt
// history are chorus's own additions on top of it.

func typeRunes(t *testing.T, m Model, s string) Model {
	t.Helper()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return updated.(Model)
}

func pressKey(t *testing.T, m Model, kt tea.KeyType) Model {
	t.Helper()
	updated, _ := m.Update(tea.KeyMsg{Type: kt})
	return updated.(Model)
}

func TestModel_CtrlJInsertsNewlineInsteadOfSubmitting(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	m = typeRunes(t, m, "line one")
	m = pressKey(t, m, tea.KeyCtrlJ)
	m = typeRunes(t, m, "line two")

	if got, want := m.input.Value(), "line one\nline two"; got != want {
		t.Fatalf("input.Value() = %q, want %q — ctrl+j should insert a newline, not submit", got, want)
	}
	select {
	case <-workers["claude"].in:
		t.Fatal("ctrl+j must not submit the prompt (enter is the only submit key)")
	default:
	}
}

func TestModel_HomeEndMoveCursorToLineStartAndEnd(t *testing.T) {
	cases := []struct {
		name             string
		startKey, endKey tea.KeyType
	}{
		{"home/end", tea.KeyHome, tea.KeyEnd},
		{"ctrl+a/ctrl+e (macOS readline bindings)", tea.KeyCtrlA, tea.KeyCtrlE},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			workers := map[string]*AgentWorker{"claude": newWorker()}
			m := newTestModel(t, workers, policy.Routing{})

			m = typeRunes(t, m, "abc")
			m = pressKey(t, m, c.startKey)
			m = typeRunes(t, m, "X")
			if got, want := m.input.Value(), "Xabc"; got != want {
				t.Fatalf("after start-of-line + typing X: input.Value() = %q, want %q", got, want)
			}

			m = pressKey(t, m, c.endKey)
			m = typeRunes(t, m, "Y")
			if got, want := m.input.Value(), "XabcY"; got != want {
				t.Fatalf("after end-of-line + typing Y: input.Value() = %q, want %q", got, want)
			}
		})
	}
}

func TestModel_ArrowUpDownAtInputBoundsWalksPromptHistory(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	m, _ = enterWithInput(m, "claude: first")
	m, _ = enterWithInput(m, "claude: second")
	m = typeRunes(t, m, "in progress draft")

	m = pressKey(t, m, tea.KeyUp)
	if got, want := m.input.Value(), "claude: second"; got != want {
		t.Fatalf("after Up from an in-progress draft: input.Value() = %q, want the most recent history entry %q", got, want)
	}

	m = pressKey(t, m, tea.KeyUp)
	if got, want := m.input.Value(), "claude: first"; got != want {
		t.Fatalf("after a second Up: input.Value() = %q, want the older history entry %q", got, want)
	}

	// Already at the oldest entry — one more Up must not go further/panic.
	m = pressKey(t, m, tea.KeyUp)
	if got, want := m.input.Value(), "claude: first"; got != want {
		t.Fatalf("Up at the oldest history entry: input.Value() = %q, want it to stay at %q", got, want)
	}

	m = pressKey(t, m, tea.KeyDown)
	if got, want := m.input.Value(), "claude: second"; got != want {
		t.Fatalf("after Down: input.Value() = %q, want %q", got, want)
	}
	m = pressKey(t, m, tea.KeyDown)
	if got, want := m.input.Value(), "in progress draft"; got != want {
		t.Fatalf("after Down past the newest history entry: input.Value() = %q, want the original draft restored", got)
	}
}

func TestModel_ArrowUpMovesWithinMultiLineDraftBeforeRecallingHistory(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m, _ = enterWithInput(m, "claude: earlier prompt")

	m = typeRunes(t, m, "second line")
	m = pressKey(t, m, tea.KeyCtrlJ)
	m = typeRunes(t, m, "third line")

	// Cursor is on the last of 3 logical lines — Up should move the cursor
	// within the draft, not touch history yet.
	m = pressKey(t, m, tea.KeyUp)
	if got, want := m.input.Value(), "second line\nthird line"; got != want {
		t.Fatalf("Up while not at the top line changed the draft: input.Value() = %q, want unchanged %q", got, want)
	}
	if m.historyIndex != -1 {
		t.Fatalf("historyIndex = %d after Up within the draft, want -1 (history untouched)", m.historyIndex)
	}
}

func TestModel_ContinuationLinePromptIsBlankNotRepeated(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	m = typeRunes(t, m, "line one")
	m = pressKey(t, m, tea.KeyCtrlJ)
	m = typeRunes(t, m, "line two")

	view := m.input.View()
	if got := strings.Count(view, "❯"); got != 1 {
		t.Fatalf("input.View() contains %d \"❯\" glyph(s) across a 2-line prompt, want exactly 1 — continuation lines should get a blank prompt of the same width, not repeat \"❯ \": %q", got, view)
	}
}

func TestModel_PromptDoneMsg_AppendsFinishedLineOnSuccessOnly(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	updated, _ := m.Update(promptDoneMsg{PromptDoneMsg{Agent: "claude", Duration: 2*time.Second + 500*time.Millisecond}})
	m = updated.(Model)
	if !strings.Contains(m.viewport.View(), "finished in") {
		t.Fatalf("viewport.View() = %q, want a \"finished in\" line after a successful prompt", m.viewport.View())
	}

	blocksBefore := len(m.blocks)
	updated, _ = m.Update(promptDoneMsg{PromptDoneMsg{Agent: "claude", Duration: time.Second, Err: context.DeadlineExceeded}})
	m = updated.(Model)
	if len(m.blocks) != blocksBefore {
		t.Fatal("a failed prompt's promptDoneMsg appended a block — want no \"finished in\" line when Err != nil, since errChMsg already reports the failure")
	}
}

func TestModel_FormatBusyStatus(t *testing.T) {
	w := newWorker()
	workers := map[string]*AgentWorker{"claude": w}
	m := newTestModel(t, workers, policy.Routing{})
	m.agentSpecs = []session.Spec{{Name: "claude"}}

	if got := m.formatBusyStatus(); got != "" {
		t.Fatalf("formatBusyStatus() = %q, want empty when no agent is busy", got)
	}

	w.startedAt.Store(time.Now().Add(-5 * time.Second))
	w.busy.Store(true)

	got := m.formatBusyStatus()
	if !strings.Contains(got, "claude") {
		t.Fatalf("formatBusyStatus() = %q, want it to mention the busy agent", got)
	}
	if !strings.Contains(got, "5s") {
		t.Fatalf("formatBusyStatus() = %q, want it to show roughly the elapsed time", got)
	}
}

// --- auto-compaction (chorus-spec.md §0's 2026-08-25 entry) -------------

func usageUpdateMsg(agent string, used, size int) outputMsg {
	return outputMsg{u: bus.Update{
		Agent: agent,
		Notification: acp.SessionNotification{
			SessionId: "main-session",
			Update:    acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{Used: used, Size: size}},
		},
	}}
}

func compactionEnabled(threshold int) policy.Compaction {
	on := true
	return policy.Compaction{Enabled: &on, ThresholdPercent: threshold}
}

func TestModel_Compaction_NoFireBelowThreshold(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.compaction = compactionEnabled(60)
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "compact"}}}

	updated, _ := m.Update(usageUpdateMsg("claude", 50, 100)) // 50%
	m = updated.(Model)

	select {
	case <-workers["claude"].in:
		t.Fatal("compaction fired below threshold")
	default:
	}
}

func TestModel_Compaction_FiresImmediatelyWhenIdle(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.compaction = compactionEnabled(60)
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "compact"}}}

	updated, _ := m.Update(usageUpdateMsg("claude", 70, 100)) // 70% >= 60%
	m = updated.(Model)

	select {
	case blocks := <-workers["claude"].in:
		if len(blocks) != 1 || blocks[0].Text == nil || blocks[0].Text.Text != "/compact" {
			t.Fatalf("queued blocks = %+v, want a single \"/compact\" text block", blocks)
		}
	default:
		t.Fatal("compaction never fired at/above threshold while the worker was idle")
	}
	if !m.compactTriggered["claude"] {
		t.Fatal("compactTriggered[claude] = false after firing, want true")
	}
}

func TestModel_Compaction_DefersUntilIdleWhenBusy(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.compaction = compactionEnabled(60)
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "compact"}}}
	workers["claude"].busy.Store(true)

	updated, _ := m.Update(usageUpdateMsg("claude", 70, 100))
	m = updated.(Model)

	select {
	case <-workers["claude"].in:
		t.Fatal("compaction fired immediately while the worker was busy, want it deferred")
	default:
	}
	if !m.compactPending["claude"] {
		t.Fatal("compactPending[claude] = false, want true while deferred")
	}

	// The turn finishes (worker goes idle) — promptDoneMsg's success path
	// must fire the deferred compaction.
	workers["claude"].busy.Store(false)
	updated, _ = m.Update(promptDoneMsg{PromptDoneMsg{Agent: "claude", Duration: time.Second}})
	m = updated.(Model)

	select {
	case blocks := <-workers["claude"].in:
		if len(blocks) != 1 || blocks[0].Text == nil || blocks[0].Text.Text != "/compact" {
			t.Fatalf("queued blocks = %+v, want a single \"/compact\" text block", blocks)
		}
	default:
		t.Fatal("deferred compaction never fired once the worker went idle")
	}
	if m.compactPending["claude"] {
		t.Fatal("compactPending[claude] = true after firing, want cleared")
	}
}

func TestModel_Compaction_OneShotUntilUsageDrops(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.compaction = compactionEnabled(60)
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "compact"}}}

	updated, _ := m.Update(usageUpdateMsg("claude", 70, 100))
	m = updated.(Model)
	select {
	case <-workers["claude"].in:
	default:
		t.Fatal("expected the first crossing to fire")
	}

	// A second update still above threshold must NOT re-fire.
	updated, _ = m.Update(usageUpdateMsg("claude", 75, 100))
	m = updated.(Model)
	select {
	case <-workers["claude"].in:
		t.Fatal("compaction re-fired on a second above-threshold update, want one-shot suppression")
	default:
	}

	// Usage drops back below threshold, then crosses again — must re-arm.
	updated, _ = m.Update(usageUpdateMsg("claude", 20, 100))
	m = updated.(Model)
	if m.compactTriggered["claude"] {
		t.Fatal("compactTriggered[claude] = true after dropping below threshold, want cleared")
	}
	updated, _ = m.Update(usageUpdateMsg("claude", 65, 100))
	m = updated.(Model)
	select {
	case <-workers["claude"].in:
	default:
		t.Fatal("compaction never re-fired after usage dropped and crossed threshold again")
	}
}

func TestModel_TrackUsage_SnapshotsIntoStatsForStatsCommand(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	updated, _ := m.Update(usageUpdateMsg("claude", 45231, 200000))
	m = updated.(Model)

	if got := m.stats.usageUsed["claude"]; got != 45231 {
		t.Fatalf("stats.usageUsed[claude] = %d, want 45231", got)
	}
	if got := m.stats.usageSize["claude"]; got != 200000 {
		t.Fatalf("stats.usageSize[claude] = %d, want 200000", got)
	}

	// A later, lower usage_update overwrites rather than accumulates —
	// ACP's Used is already a cumulative session total, not a per-turn
	// delta, so trackUsage snapshots (last value wins), never sums.
	updated, _ = m.Update(usageUpdateMsg("claude", 10000, 200000))
	m = updated.(Model)
	if got := m.stats.usageUsed["claude"]; got != 10000 {
		t.Fatalf("stats.usageUsed[claude] = %d after a second update, want 10000 (overwritten, not summed)", got)
	}
}

func TestModel_Compaction_NoCommandDiscoveredSaysSoInsteadOfGuessing(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.compaction = compactionEnabled(60)
	// No commands registered at all for claude.

	updated, _ := m.Update(usageUpdateMsg("claude", 70, 100))
	m = updated.(Model)

	select {
	case blocks := <-workers["claude"].in:
		t.Fatalf("queued blocks = %+v, want nothing sent when no compaction command was discovered", blocks)
	default:
	}
	if !strings.Contains(m.viewport.View(), "no compaction-like command was discovered") {
		t.Fatalf("viewport.View() = %q, want a message explaining nothing was sent", m.viewport.View())
	}
}

func TestModel_Compaction_DisabledByDefault(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{}) // compaction left zero-valued
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "compact"}}}

	updated, _ := m.Update(usageUpdateMsg("claude", 99, 100))
	m = updated.(Model)

	select {
	case <-workers["claude"].in:
		t.Fatal("compaction fired despite EnabledOrDefault() being false")
	default:
	}
}

// --- LLM-based routing + context handoff (chorus-spec.md §0's
// 2026-08-25 entry) ------------------------------------------------------

func TestModel_AppendActivity_MergesConsecutiveSameKeyEntries(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.appendActivity("agent:claude", "hello ")
	m.appendActivity("agent:claude", "world")
	m.appendActivity("user:opencode", "next prompt")

	if len(m.activity) != 2 {
		t.Fatalf("len(activity) = %d, want 2 (two chunks from the same key merged into one entry)", len(m.activity))
	}
	if m.activity[0].text != "hello world" {
		t.Fatalf("activity[0].text = %q, want merged \"hello world\"", m.activity[0].text)
	}
	if m.activity[1].key != "user:opencode" || m.activity[1].text != "next prompt" {
		t.Fatalf("activity[1] = %+v, want a new entry for the different key", m.activity[1])
	}
}

func TestModel_ActivityContext_Tiers(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	for i := 0; i < activityDigestN+3; i++ {
		m.appendActivity(fmt.Sprintf("user:claude:%d", i), fmt.Sprintf("entry %d", i))
	}

	if got := m.activityContext("prompt"); got != "" {
		t.Fatalf("activityContext(\"prompt\") = %q, want empty", got)
	}
	digest := m.activityContext("digest")
	if strings.Contains(digest, "entry 0") {
		t.Fatalf("activityContext(\"digest\") = %q, want only the most recent %d entries, not the oldest", digest, activityDigestN)
	}
	if !strings.Contains(digest, fmt.Sprintf("entry %d", activityDigestN+2)) {
		t.Fatalf("activityContext(\"digest\") = %q, want the most recent entry present", digest)
	}
	full := m.activityContext("full")
	if !strings.Contains(full, "entry 0") {
		t.Fatalf("activityContext(\"full\") = %q, want the oldest entry still present (within cap)", full)
	}
}

// A long prior turn (a multi-phase plan) must survive a handoff intact —
// the exact regression that motivated storing near-verbatim and budgeting
// at render, instead of the old flat 1000-char/entry cap that dropped
// everything past "Phase 1" the moment another agent picked the work up.
func TestModel_ActivityContext_FullTierCarriesLongPriorTurn(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	plan := "Phase 1: scaffold. " + strings.Repeat("Phase N: more detail. ", 300) + "Phase Final: ship it."
	if len(plan) <= 1000 {
		t.Fatalf("test plan is only %d chars — it must exceed the old 1000-char cap to be meaningful", len(plan))
	}
	m.appendActivity("agent:gemini", plan)

	full := m.activityContext("full")
	if !strings.Contains(full, "Phase Final: ship it.") {
		t.Fatal("activityContext(\"full\") dropped the end of a long prior turn — a handoff would lose everything past the start")
	}
}

// The digest tier (what the router reads to decide) truncates each entry at
// render so a long stored turn doesn't inflate every routing decision — the
// "keep the router cheap" half of decoupling storage fidelity from render.
func TestModel_ActivityContext_DigestTruncatesLongEntryToStayCheap(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.appendActivity("agent:gemini", strings.Repeat("x", 5000))

	digest := m.activityContext("digest")
	if len(digest) > activityDigestEntryCap+200 {
		t.Fatalf("digest render = %d chars, want it truncated near activityDigestEntryCap (%d) so routing stays cheap", len(digest), activityDigestEntryCap)
	}
	if !strings.Contains(digest, "...") {
		t.Fatal("digest render of a long entry has no truncation marker — it wasn't capped")
	}
}

// The full tier holds its render within activityHandoffBudget, dropping the
// oldest entries first while always keeping the most recent — so a very long
// session's handoff can't blow the receiving agent's context window, yet
// still carries the freshest work.
func TestModel_ActivityContext_FullTierBudgetDropsOldestKeepsNewest(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	big := strings.Repeat("y", 5000)
	n := activityHandoffBudget/5000 + 5 // enough entries to overflow the budget
	for i := 0; i < n; i++ {
		m.appendActivity(fmt.Sprintf("agent:claude:%d", i), fmt.Sprintf("START-%d-%s", i, big))
	}

	full := m.activityContext("full")
	if len(full) > activityHandoffBudget+6000 {
		t.Fatalf("full render = %d chars, want it held within activityHandoffBudget (%d)", len(full), activityHandoffBudget)
	}
	if !strings.Contains(full, fmt.Sprintf("START-%d-", n-1)) {
		t.Fatal("full render dropped the most recent entry — budget trimming must keep newest")
	}
	if strings.Contains(full, "START-0-") {
		t.Fatal("full render still contains the oldest entry — budget trimming should drop oldest first")
	}
}

func TestModel_StartRouteDecision_FallsBackSynchronouslyWhenDecisionAgentNotConnected(t *testing.T) {
	workers := map[string]*AgentWorker{"opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{Mode: string(policy.RoutingLLM), DecisionAgent: "claude"})
	m.defaultAgent = "opencode"
	// m.conns has no "claude" entry — decision_agent isn't connected.

	updated, cmd := m.startRouteDecision("do something")
	m = updated.(Model)
	if cmd != nil {
		t.Fatal("startRouteDecision() cmd != nil, want a synchronous fallback (nil cmd) when the decision agent isn't connected")
	}
	select {
	case blocks := <-workers["opencode"].in:
		if len(blocks) != 1 || blocks[0].Text == nil || blocks[0].Text.Text != "do something" {
			t.Fatalf("opencode's queued blocks = %+v, want the original prompt sent directly", blocks)
		}
	default:
		t.Fatal("opencode never received the prompt via the synchronous fallback")
	}
	if !strings.Contains(m.viewport.View(), "isn't connected") {
		t.Fatalf("viewport.View() = %q, want an explanation of the fallback", m.viewport.View())
	}
}

func TestModel_StartRouteDecision_KicksOffAsyncCallWhenDecisionAgentConnected(t *testing.T) {
	workers := map[string]*AgentWorker{"opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{Mode: string(policy.RoutingLLM), DecisionAgent: "opencode"})
	m.conns = map[string]*session.Connection{"opencode": {}}

	updated, cmd := m.startRouteDecision("do something")
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("startRouteDecision() cmd = nil, want a tea.Cmd kicked off when the decision agent is connected")
	}
	if !strings.Contains(m.viewport.View(), "asking opencode to decide") {
		t.Fatalf("viewport.View() = %q, want an in-progress indicator", m.viewport.View())
	}
	select {
	case <-workers["opencode"].in:
		t.Fatal("nothing should be queued directly — the actual prompt is sent once routeDecisionMsg comes back")
	default:
	}
}

// --- routing prompt echo ordering (2026-09-03, user report) -------------
//
// Previously "[routing] asking X to decide..." appeared before the user's
// own prompt was echoed at all (the echo only happened once the decision
// resolved and the real target agent was known) — reading backwards, as
// if chorus were reacting to a question not yet shown. Fixed by echoing
// under a neutral "routing" tag immediately in startRouteDecision, before
// the decision even starts, and suppressing dispatchUserTurn's own later
// echo (echo=false) so the prompt doesn't then show a second time once
// the real agent is known.

func TestModel_StartRouteDecision_EchoesPromptBeforeAskingDecisionAgent(t *testing.T) {
	workers := map[string]*AgentWorker{"opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{Mode: string(policy.RoutingLLM), DecisionAgent: "opencode"})
	m.conns = map[string]*session.Connection{"opencode": {}}

	updated, _ := m.startRouteDecision("write an elevator pitch")
	m = updated.(Model)

	view := m.viewport.View()
	echoIdx := strings.Index(view, "write an elevator pitch")
	askIdx := strings.Index(view, "asking opencode to decide")
	if echoIdx == -1 {
		t.Fatalf("viewport.View() = %q, want the prompt echoed", view)
	}
	if askIdx == -1 {
		t.Fatalf("viewport.View() = %q, want the routing in-progress line", view)
	}
	if echoIdx >= askIdx {
		t.Fatalf("prompt echo (index %d) does not come before \"asking ... to decide\" (index %d) — want the prompt shown first", echoIdx, askIdx)
	}
}

func TestModel_HandleRouteDecision_DoesNotEchoPromptASecondTime(t *testing.T) {
	workers := map[string]*AgentWorker{"opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{Mode: string(policy.RoutingLLM), DecisionAgent: "opencode"})
	m.conns = map[string]*session.Connection{"opencode": {}}

	updated, _ := m.startRouteDecision("write an elevator pitch")
	m = updated.(Model)
	updated, _ = m.Update(routeDecisionMsg{prompt: "write an elevator pitch", decision: router.Decision{Agent: "opencode"}})
	m = updated.(Model)

	if n := strings.Count(m.viewport.View(), "write an elevator pitch"); n != 1 {
		t.Fatalf("prompt text appears %d times in viewport.View(), want exactly 1 (echoed once under \"routing\", not again once the target agent is known)", n)
	}
}

func TestModel_StartRouteDecision_FallbackAlsoEchoesPromptOnceNotTwice(t *testing.T) {
	workers := map[string]*AgentWorker{"opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{Mode: string(policy.RoutingLLM), DecisionAgent: "claude"})
	m.defaultAgent = "opencode"
	// m.conns has no "claude" entry — decision_agent isn't connected, same
	// synchronous-fallback setup as the existing test above this one.

	updated, _ := m.startRouteDecision("do something")
	m = updated.(Model)

	view := m.viewport.View()
	if n := strings.Count(view, "do something"); n != 1 {
		t.Fatalf("prompt text appears %d times in viewport.View(), want exactly 1", n)
	}
	echoIdx := strings.Index(view, "do something")
	fallbackIdx := strings.Index(view, "isn't connected")
	if echoIdx == -1 || fallbackIdx == -1 || echoIdx >= fallbackIdx {
		t.Fatalf("viewport.View() = %q, want the prompt echoed before the fallback explanation", view)
	}
}

func TestModel_HandleRouteDecision_ErrorFallsBackToDefaultAgent(t *testing.T) {
	workers := map[string]*AgentWorker{"opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.defaultAgent = "opencode"

	updated, _ := m.Update(routeDecisionMsg{prompt: "do something", err: errFixture("parse failed")})
	m = updated.(Model)

	select {
	case blocks := <-workers["opencode"].in:
		if len(blocks) != 1 || blocks[0].Text == nil || blocks[0].Text.Text != "do something" {
			t.Fatalf("queued blocks = %+v, want the original prompt sent to the default agent", blocks)
		}
	default:
		t.Fatal("default agent never received the prompt after a decision error")
	}
	if !strings.Contains(m.viewport.View(), "decision failed") {
		t.Fatalf("viewport.View() = %q, want an explanation", m.viewport.View())
	}
}

func TestModel_HandleRouteDecision_UnknownAgentFallsBackToDefaultAgent(t *testing.T) {
	workers := map[string]*AgentWorker{"opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.defaultAgent = "opencode"

	updated, _ := m.Update(routeDecisionMsg{prompt: "do something", decision: router.Decision{Agent: "gemini"}})
	m = updated.(Model)

	select {
	case blocks := <-workers["opencode"].in:
		if blocks[0].Text.Text != "do something" {
			t.Fatalf("queued blocks = %+v, want the fallback to the default agent", blocks)
		}
	default:
		t.Fatal("default agent never received the prompt after an unknown-agent decision")
	}
}

func TestModel_HandleRouteDecision_NoHandoffWhenSameAgentContinues(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.lastRoutedAgent = "claude"

	updated, _ := m.Update(routeDecisionMsg{prompt: "keep going", decision: router.Decision{Agent: "claude"}})
	m = updated.(Model)

	select {
	case blocks := <-workers["claude"].in:
		if blocks[0].Text.Text != "keep going" {
			t.Fatalf("queued text = %q, want the bare prompt with no handoff preamble when the agent didn't change", blocks[0].Text.Text)
		}
	default:
		t.Fatal("claude never received the prompt")
	}
}

func TestModel_HandleRouteDecision_HandoffPreambleOnAgentSwitch(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker(), "opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.lastRoutedAgent = "opencode"
	m.appendActivity("agent:opencode", "opencode did some earlier work")

	updated, _ := m.Update(routeDecisionMsg{prompt: "continue the task", decision: router.Decision{Agent: "claude"}})
	m = updated.(Model)

	select {
	case blocks := <-workers["claude"].in:
		text := blocks[0].Text.Text
		if !strings.Contains(text, "automated handoff context from chorus") {
			t.Fatalf("queued text = %q, want the self-identifying handoff preamble", text)
		}
		if !strings.Contains(text, "opencode did some earlier work") {
			t.Fatalf("queued text = %q, want the prior activity included", text)
		}
		if !strings.Contains(text, "continue the task") {
			t.Fatalf("queued text = %q, want the actual prompt still present", text)
		}
	default:
		t.Fatal("claude never received the handed-off prompt")
	}
	if m.lastRoutedAgent != "claude" {
		t.Fatalf("lastRoutedAgent = %q, want claude after the switch", m.lastRoutedAgent)
	}
}

// An explicit "<agent>: text" switch gets the same handoff preamble the LLM
// router's switches do — the newly-selected agent has its own isolated ACP
// session and no memory of the prior turns regardless of HOW it was
// selected, so the context has to travel either way.
func TestModel_ExplicitSwitch_HandoffPreambleOnAgentSwitch(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker(), "opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.lastRoutedAgent = "opencode"
	m.appendActivity("agent:opencode", "opencode did some earlier work")

	m, _ = enterWithInput(m, "claude: continue the task")

	select {
	case blocks := <-workers["claude"].in:
		text := blocks[0].Text.Text
		if !strings.Contains(text, "automated handoff context from chorus") {
			t.Fatalf("queued text = %q, want the self-identifying handoff preamble", text)
		}
		if !strings.Contains(text, "opencode did some earlier work") {
			t.Fatalf("queued text = %q, want the prior activity included", text)
		}
		if !strings.Contains(text, "continue the task") {
			t.Fatalf("queued text = %q, want the actual prompt still present", text)
		}
	default:
		t.Fatal("claude never received the handed-off prompt")
	}
	if m.lastRoutedAgent != "claude" {
		t.Fatalf("lastRoutedAgent = %q, want claude after the switch", m.lastRoutedAgent)
	}
	// The echo shows the user their bare prompt, not the preamble scaffolding.
	if strings.Contains(m.viewport.View(), "automated handoff context from chorus") {
		t.Fatal("viewport shows the handoff preamble — it should only go to the agent, not be echoed to the user")
	}
}

func TestModel_ExplicitSwitch_NoHandoffWhenSameAgentContinues(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.lastRoutedAgent = "claude"

	m, _ = enterWithInput(m, "claude: keep going")

	select {
	case blocks := <-workers["claude"].in:
		if blocks[0].Text.Text != "keep going" {
			t.Fatalf("queued text = %q, want the bare prompt with no handoff preamble when the agent didn't change", blocks[0].Text.Text)
		}
	default:
		t.Fatal("claude never received the prompt")
	}
}

// A slash command routed to a different agent is a control command, not a
// conversational turn — it must NOT be wrapped in a handoff preamble (that
// would corrupt the command the agent parses).
func TestModel_SlashCommandSwitch_NoHandoffPreamble(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker(), "opencode": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.lastRoutedAgent = "opencode"
	m.appendActivity("agent:opencode", "opencode did some earlier work")
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "plan"}}}

	m, _ = enterWithInput(m, "/plan the migration")

	select {
	case blocks := <-workers["claude"].in:
		text := blocks[0].Text.Text
		if strings.Contains(text, "automated handoff context from chorus") {
			t.Fatalf("queued text = %q, want the bare slash command with no handoff preamble", text)
		}
		if text != "/plan the migration" {
			t.Fatalf("queued text = %q, want the slash command sent verbatim", text)
		}
	default:
		t.Fatal("claude never received the slash command")
	}
}

func TestModel_HandleRouteDecision_ModelSwitchQueuesTwoTurnsInOrder(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.lastRoutedAgent = "claude" // no handoff, isolate the model-switch behavior
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "model"}}}

	updated, _ := m.Update(routeDecisionMsg{prompt: "do the hard part", decision: router.Decision{Agent: "claude", Model: "claude-opus-4-8"}})
	m = updated.(Model)

	first := <-workers["claude"].in
	if first[0].Text == nil || first[0].Text.Text != "/model claude-opus-4-8" {
		t.Fatalf("first queued turn = %+v, want the model-switch command first", first)
	}
	select {
	case second := <-workers["claude"].in:
		if second[0].Text == nil || second[0].Text.Text != "do the hard part" {
			t.Fatalf("second queued turn = %+v, want the actual prompt second", second)
		}
	default:
		t.Fatal("only one turn was queued, want the model-switch command followed by the prompt")
	}
}

func TestModel_HandleRouteDecision_NoModelCommandDiscoveredSkipsSwitchSilently(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.lastRoutedAgent = "claude"
	// No commands registered — no model-switch command discoverable.

	updated, _ := m.Update(routeDecisionMsg{prompt: "do it", decision: router.Decision{Agent: "claude", Model: "claude-opus-4-8"}})
	m = updated.(Model)

	blocks := <-workers["claude"].in
	if blocks[0].Text.Text != "do it" {
		t.Fatalf("queued text = %q, want just the prompt (no model-switch attempt when no command was discovered)", blocks[0].Text.Text)
	}
	select {
	case extra := <-workers["claude"].in:
		t.Fatalf("unexpected second queued turn = %+v", extra)
	default:
	}
}

func TestModel_ContextCommand_OpensMenuArrowSelectsAndSetsLevel(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{ContextLevel: "digest"})

	m, _ = enterWithInput(m, "context")
	if m.mode() != modeContextLevel {
		t.Fatalf("mode() = %v, want modeContextLevel after the context command", m.mode())
	}
	if m.contextCursor != 1 { // "digest" is index 1
		t.Fatalf("contextCursor = %d, want 1 (starting at the current level, digest)", m.contextCursor)
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.contextCursor != 2 { // "full"
		t.Fatalf("contextCursor = %d, want 2 (full) after one Down from digest", m.contextCursor)
	}

	m, _ = enterWithInput(m, "")
	if m.mode() != modeNormal {
		t.Fatalf("mode() = %v, want modeNormal once the menu is answered", m.mode())
	}
	if m.routing.ContextLevel != "full" {
		t.Fatalf("routing.ContextLevel = %q, want full", m.routing.ContextLevel)
	}
	if !strings.Contains(m.viewport.View(), `"full"`) {
		t.Fatalf("viewport.View() = %q, want confirmation of the new level", m.viewport.View())
	}
}

// --- Esc-to-interrupt (selectInterruptTargets / busyAgentNames) ---------

func TestSelectInterruptTargets_PrefersLastRoutedWhenBusy(t *testing.T) {
	got := selectInterruptTargets([]string{"claude", "opencode"}, "opencode")
	if len(got) != 1 || got[0] != "opencode" {
		t.Fatalf("selectInterruptTargets = %v, want [opencode] (the busy last-routed agent alone)", got)
	}
}

func TestSelectInterruptTargets_FallsBackToAllBusyWhenLastRoutedIdleOrUnset(t *testing.T) {
	got := selectInterruptTargets([]string{"claude", "opencode"}, "gemini")
	if len(got) != 2 || got[0] != "claude" || got[1] != "opencode" {
		t.Fatalf("selectInterruptTargets = %v, want [claude opencode] since lastRouted (gemini) isn't among the busy agents", got)
	}

	got = selectInterruptTargets([]string{"claude"}, "")
	if len(got) != 1 || got[0] != "claude" {
		t.Fatalf("selectInterruptTargets = %v, want [claude] when lastRouted is unset", got)
	}
}

func TestSelectInterruptTargets_EmptyBusyReturnsEmpty(t *testing.T) {
	if got := selectInterruptTargets(nil, "claude"); len(got) != 0 {
		t.Fatalf("selectInterruptTargets = %v, want empty when nothing is busy", got)
	}
}

func TestModel_BusyAgentNames_ReturnsOnlyBusyOnesInRegistryOrder(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker(), "opencode": newWorker(), "gemini": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.agentSpecs = []session.Spec{{Name: "claude"}, {Name: "opencode"}, {Name: "gemini"}}

	if got := m.busyAgentNames(); len(got) != 0 {
		t.Fatalf("busyAgentNames() = %v, want empty when no worker is busy", got)
	}

	workers["gemini"].busy.Store(true)
	workers["claude"].busy.Store(true)
	got := m.busyAgentNames()
	if len(got) != 2 || got[0] != "claude" || got[1] != "gemini" {
		t.Fatalf("busyAgentNames() = %v, want [claude gemini] in registry order, not busy-call order", got)
	}
}

func TestModel_InterruptBusyAgents_NoOpWhenNothingIsBusy(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.agentSpecs = []session.Spec{{Name: "claude"}}

	// Must not panic even though the test worker has no real session.AgentSession
	// backing it (newWorker() leaves sess nil) — interruptBusyAgents should
	// never reach AgentWorker.Cancel when busyAgentNames() is empty.
	updated := m.interruptBusyAgents()
	if len(updated.blocks) != len(m.blocks) {
		t.Fatalf("interruptBusyAgents() on an idle model changed the document (%d -> %d blocks), want no-op", len(m.blocks), len(updated.blocks))
	}
}

func TestModel_EscKeyInterruptsOnlyInNormalMode(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.agentSpecs = []session.Spec{{Name: "claude"}}

	// modeNormal, nothing busy: Esc must be handled (not fall through to the
	// textarea, which would insert nothing anyway, but confirms the KeyEsc
	// case is actually wired up) and must not panic.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.mode() != modeNormal {
		t.Fatalf("mode() = %v after Esc with nothing pending, want modeNormal", m.mode())
	}
}

// --- click-drag selection + copy-on-select (extractSelection / handleMouse) ---

func TestExtractSelection_SingleLine(t *testing.T) {
	doc := "alpha\nbeta\ngamma"
	text, n, ok := extractSelection(doc, 1, 1)
	if !ok || n != 1 || text != "beta" {
		t.Fatalf("extractSelection(doc,1,1) = (%q,%d,%v), want (\"beta\",1,true)", text, n, ok)
	}
}

func TestExtractSelection_RangeIsOrderIndependent(t *testing.T) {
	doc := "alpha\nbeta\ngamma\ndelta"
	forward, nf, okf := extractSelection(doc, 1, 2)
	backward, nb, okb := extractSelection(doc, 2, 1)
	if !okf || !okb || forward != backward || nf != nb {
		t.Fatalf("extractSelection forward=(%q,%d,%v) backward=(%q,%d,%v), want equal regardless of drag direction", forward, nf, okf, backward, nb, okb)
	}
	if forward != "beta\ngamma" {
		t.Fatalf("extractSelection(doc,1,2) = %q, want \"beta\\ngamma\"", forward)
	}
}

func TestExtractSelection_ClampsOutOfRangeIndices(t *testing.T) {
	doc := "alpha\nbeta"
	text, n, ok := extractSelection(doc, -5, 50)
	if !ok || n != 2 || text != "alpha\nbeta" {
		t.Fatalf("extractSelection(doc,-5,50) = (%q,%d,%v), want the whole clamped doc", text, n, ok)
	}
}

func TestExtractSelection_StripsANSI(t *testing.T) {
	doc := "\x1b[31mred\x1b[0m line"
	text, n, ok := extractSelection(doc, 0, 0)
	if !ok || n != 1 || text != "red line" {
		t.Fatalf("extractSelection with ANSI codes = (%q,%d,%v), want (\"red line\",1,true) — copied text must be plain", text, n, ok)
	}
}

// stubClipboard replaces writeClipboard for the duration of a test so
// copy-on-select tests never touch the real OS clipboard (see
// writeClipboard's doc comment) — captures the last text written, or
// returns a configured error.
func stubClipboard(t *testing.T, err error) *string {
	t.Helper()
	orig := writeClipboard
	var got string
	writeClipboard = func(text string) error {
		got = text
		return err
	}
	t.Cleanup(func() { writeClipboard = orig })
	return &got
}

func TestModel_HandleMouse_PressDragReleaseCopiesSelection(t *testing.T) {
	got := stubClipboard(t, nil)
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.appendLine("one\n")
	m.appendLine("two\n")
	m.appendLine("three\n")
	m.syncViewport()

	updated, _ := m.handleMouse(tea.MouseMsg{Y: 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)
	if !m.selecting {
		t.Fatal("selecting = false after a left-button press inside the viewport, want true")
	}

	updated, _ = m.handleMouse(tea.MouseMsg{Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
	m = updated.(Model)
	if m.selectCurLine != m.selectAnchorLine+1 {
		t.Fatalf("selectCurLine = %d, want anchor+1 (%d) after dragging down one row", m.selectCurLine, m.selectAnchorLine+1)
	}

	updated, _ = m.handleMouse(tea.MouseMsg{Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease})
	m = updated.(Model)
	if m.selecting {
		t.Fatal("selecting = true after release, want false")
	}
	if *got != "one\ntwo" {
		t.Fatalf("clipboard received %q, want \"one\\ntwo\"", *got)
	}
	if m.lastCopyStatus != "2 lines copied" {
		t.Fatalf("lastCopyStatus = %q, want \"2 lines copied\"", m.lastCopyStatus)
	}
}

// highlightSGR is render's HighlightLine background code — checked
// directly against the raw viewport output rather than via a helper,
// since the point of these tests is confirming the ANSI actually reaches
// the rendered screen content, not just that some internal flag got set.
const highlightSGR = "\x1b[100m"

func TestModel_HandleMouse_DragHighlightsSelectedLinesInViewport(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.appendLine("one\n")
	m.appendLine("two\n")
	m.appendLine("three\n")
	m.syncViewport()
	if strings.Contains(m.viewport.View(), highlightSGR) {
		t.Fatal("viewport already contains a highlight before any selection started")
	}

	updated, _ := m.handleMouse(tea.MouseMsg{Y: 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)
	if !strings.Contains(m.viewport.View(), highlightSGR) {
		t.Fatal("viewport has no highlight immediately after a press starts a selection — a drag must be visible as it happens, not only after release")
	}

	updated, _ = m.handleMouse(tea.MouseMsg{Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
	m = updated.(Model)
	view := m.viewport.View()
	if !strings.Contains(view, "one") || !strings.Contains(view, "two") {
		t.Fatalf("viewport = %q, want both dragged-over lines still present (just re-styled)", view)
	}
	if n := strings.Count(view, highlightSGR); n != 2 {
		t.Fatalf("highlightSGR appears %d times after dragging over 2 lines, want exactly 2", n)
	}
}

func TestModel_HandleMouse_HighlightPersistsAfterReleaseThenClearsOnNextKey(t *testing.T) {
	stubClipboard(t, nil)
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.appendLine("one\n")
	m.appendLine("two\n")
	m.syncViewport()

	updated, _ := m.handleMouse(tea.MouseMsg{Y: 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)
	updated, _ = m.handleMouse(tea.MouseMsg{Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
	m = updated.(Model)
	updated, _ = m.handleMouse(tea.MouseMsg{Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease})
	m = updated.(Model)

	if !strings.Contains(m.viewport.View(), highlightSGR) {
		t.Fatal("highlight disappeared immediately on release, want it to persist as confirmation of what was just copied")
	}

	// Any keypress dismisses it, same one-shot lifespan as lastCopyStatus.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = updated.(Model)
	if strings.Contains(m.viewport.View(), highlightSGR) {
		t.Fatal("highlight still present after a keypress, want it cleared")
	}
}

func TestModel_ApplySelectionHighlight_OnlyRestylesTheSelectedRange(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.selectAnchorLine, m.selectCurLine = 1, 1
	got := m.applySelectionHighlight("alpha\nbeta\ngamma")
	want := "alpha\n" + highlightSGR + " beta" + strings.Repeat(" ", m.viewport.Width-5) + "\x1b[0m\ngamma"
	if got != want {
		t.Fatalf("applySelectionHighlight() = %q, want %q", got, want)
	}
}

func TestModel_HandleMouse_PressOutsideViewportDoesNotSelect(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})

	updated, _ := m.handleMouse(tea.MouseMsg{Y: m.viewport.Height + 5, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)
	if m.selecting {
		t.Fatal("selecting = true after a press below the viewport (in the status/input area), want false")
	}
}

func TestModel_HandleMouse_WheelEventsStillReachViewport(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	for i := 0; i < 30; i++ {
		m.appendLine(fmt.Sprintf("line %d\n", i))
	}
	m.syncViewport()
	m.viewport.GotoTop()

	updated, _ := m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	m = updated.(Model)
	if m.viewport.YOffset == 0 {
		t.Fatal("YOffset unchanged after a wheel-down event, want it forwarded to viewport.Update and to scroll")
	}
	if m.selecting {
		t.Fatal("selecting = true after a wheel event, want false (only left-button drags select)")
	}
}

func TestModel_CopySelection_ClipboardErrorSetsStatusInsteadOfPanicking(t *testing.T) {
	stubClipboard(t, fmt.Errorf("no clipboard utility found"))
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.appendLine("one\n")
	m.syncViewport()
	m.selectAnchorLine, m.selectCurLine = 0, 0

	m = m.copySelection()
	if !strings.Contains(m.lastCopyStatus, "copy failed") {
		t.Fatalf("lastCopyStatus = %q, want it to report the clipboard error", m.lastCopyStatus)
	}
}

func TestClampInt(t *testing.T) {
	if got := clampInt(5, 0, 10); got != 5 {
		t.Fatalf("clampInt(5,0,10) = %d, want 5", got)
	}
	if got := clampInt(-1, 0, 10); got != 0 {
		t.Fatalf("clampInt(-1,0,10) = %d, want 0", got)
	}
	if got := clampInt(20, 0, 10); got != 10 {
		t.Fatalf("clampInt(20,0,10) = %d, want 10", got)
	}
}

// --- `mode`/`auto` REPL commands (ACP session/set_mode) ------------------
//
// runSetMode's returned tea.Cmd performs a real RPC (session.AgentSession.
// SetMode -> conn.SetSessionMode) — these tests never execute a returned
// cmd (that would nil-pointer-dereference the fake worker's nil conn),
// only assert whether one was produced and what handleModeCommand/
// handleAutoCommand appended synchronously. setModeResultMsg's own
// handling (the part that runs after a real RPC would have returned) is
// tested directly by constructing the message, exactly like
// TestModel_TrackUsage_SnapshotsIntoStatsForStatsCommand does for
// usageUpdateMsg.

func workerWithModes(modes []acp.SessionMode, current acp.SessionModeId) *AgentWorker {
	w := newWorker()
	w.sess = &session.AgentSession{AvailableModes: modes, CurrentModeId: current}
	return w
}

func TestModel_ModeCommand_MissingArgumentReportsUsage(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	m, _ = enterWithInput(m, "mode claude")

	if out := m.viewport.View(); !strings.Contains(out, "usage: mode") {
		t.Fatalf("viewport.View() = %q, want a usage message for a mode command with no mode argument", out)
	}
}

func TestModel_ModeCommand_UnknownAgentReportsError(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})

	m, _ = enterWithInput(m, "mode nope acceptEdits")

	if out := m.viewport.View(); !strings.Contains(out, "unknown agent") {
		t.Fatalf("viewport.View() = %q, want an unknown-agent error", out)
	}
}

func TestModel_ModeCommand_UnmatchedModeReportsError(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": workerWithModes(
		[]acp.SessionMode{{Id: "default", Name: "Default"}}, "default")}
	m := newTestModel(t, workers, policy.Routing{})

	m, _ = enterWithInput(m, "mode claude nonexistent")

	if out := m.viewport.View(); !strings.Contains(out, "no mode matching") {
		t.Fatalf("viewport.View() = %q, want a no-match error naming `modes`", out)
	}
}

func TestModel_ModeCommand_ValidRequestReturnsSetModeCmd(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": workerWithModes(
		[]acp.SessionMode{{Id: "default", Name: "Default"}, {Id: "acceptEdits", Name: "Accept Edits"}}, "default")}
	m := newTestModel(t, workers, policy.Routing{})

	_, cmd := enterWithInput(m, "mode claude acceptEdits")

	if cmd == nil {
		t.Fatal("enterWithInput(\"mode claude acceptEdits\") returned a nil cmd, want the async runSetMode command")
	}
}

func TestModel_AutoCommand_NoAgentConfiguredReportsMessage(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.agentSpecs = []session.Spec{{Name: "claude"}} // no AutoMode set

	m, _ = enterWithInput(m, "auto")

	if out := m.viewport.View(); !strings.Contains(out, "no connected agent has auto_mode configured") {
		t.Fatalf("viewport.View() = %q, want the no-auto_mode-configured message", out)
	}
}

func TestModel_AutoCommand_UnknownAgentArgReportsError(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()}
	m := newTestModel(t, workers, policy.Routing{})
	m.agentSpecs = []session.Spec{{Name: "claude", AutoMode: "acceptEdits"}}

	m, _ = enterWithInput(m, "auto nope")

	if out := m.viewport.View(); !strings.Contains(out, "unknown agent") {
		t.Fatalf("viewport.View() = %q, want an unknown-agent error", out)
	}
}

func TestModel_AutoCommand_ValidAgentReturnsSetModeCmd(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": workerWithModes(
		[]acp.SessionMode{{Id: "default", Name: "Default"}, {Id: "acceptEdits", Name: "Accept Edits"}}, "default")}
	m := newTestModel(t, workers, policy.Routing{})
	m.agentSpecs = []session.Spec{{Name: "claude", AutoMode: "acceptEdits"}}

	_, cmd := enterWithInput(m, "auto claude")

	if cmd == nil {
		t.Fatal("enterWithInput(\"auto claude\") returned a nil cmd, want the async runSetMode command")
	}
}

func TestModel_AutoCommand_UnresolvableAutoModeReportsError(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": workerWithModes(
		[]acp.SessionMode{{Id: "default", Name: "Default"}}, "default")}
	m := newTestModel(t, workers, policy.Routing{})
	m.agentSpecs = []session.Spec{{Name: "claude", AutoMode: "doesNotExist"}}

	m, _ = enterWithInput(m, "auto claude")

	if out := m.viewport.View(); !strings.Contains(out, "doesn't match any mode it advertised") {
		t.Fatalf("viewport.View() = %q, want an error naming the unmatched auto_mode value", out)
	}
}

func TestModel_SetModeResultMsg_Success_UpdatesCurrentModeAndAppendsConfirmation(t *testing.T) {
	w := workerWithModes([]acp.SessionMode{{Id: "default", Name: "Default"}, {Id: "acceptEdits", Name: "Accept Edits"}}, "default")
	workers := map[string]*AgentWorker{"claude": w}
	m := newTestModel(t, workers, policy.Routing{})

	updated, _ := m.Update(setModeResultMsg{agent: "claude", modeId: "acceptEdits", modeLabel: "acceptEdits"})
	m = updated.(Model)

	if got := w.CurrentModeId(); got != "acceptEdits" {
		t.Fatalf("CurrentModeId() = %q after a successful setModeResultMsg, want %q (belt-and-suspenders update)", got, "acceptEdits")
	}
	if out := m.viewport.View(); !strings.Contains(out, "mode set to acceptEdits") {
		t.Fatalf("viewport.View() = %q, want a confirmation naming the new mode", out)
	}
}

func TestModel_SetModeResultMsg_Error_AppendsFailureMessageWithoutChangingMode(t *testing.T) {
	w := workerWithModes([]acp.SessionMode{{Id: "default", Name: "Default"}}, "default")
	workers := map[string]*AgentWorker{"claude": w}
	m := newTestModel(t, workers, policy.Routing{})

	updated, _ := m.Update(setModeResultMsg{agent: "claude", modeId: "acceptEdits", modeLabel: "acceptEdits", err: fmt.Errorf("boom")})
	m = updated.(Model)

	if got := w.CurrentModeId(); got != "default" {
		t.Fatalf("CurrentModeId() = %q after a failed setModeResultMsg, want unchanged %q", got, "default")
	}
	if out := m.viewport.View(); !strings.Contains(out, "mode switch failed") {
		t.Fatalf("viewport.View() = %q, want a failure message", out)
	}
}

func TestModel_SetModeResultMsg_AutoOn_RecordsPrevModeForToggleBack(t *testing.T) {
	w := workerWithModes([]acp.SessionMode{{Id: "default", Name: "Default"}, {Id: "acceptEdits", Name: "Accept Edits"}}, "default")
	workers := map[string]*AgentWorker{"claude": w}
	m := newTestModel(t, workers, policy.Routing{})

	updated, _ := m.Update(setModeResultMsg{agent: "claude", modeId: "acceptEdits", modeLabel: "acceptEdits", auto: true, turnOn: true, restoreId: "default"})
	m = updated.(Model)

	if got, ok := m.autoPrevMode["claude"]; !ok || got != "default" {
		t.Fatalf("autoPrevMode[claude] = (%q, %v), want (\"default\", true) so a second `auto` can toggle back", got, ok)
	}
	if out := m.viewport.View(); !strings.Contains(out, "auto mode on") {
		t.Fatalf("viewport.View() = %q, want an auto-mode-on confirmation", out)
	}
}

func TestModel_SetModeResultMsg_AutoOff_ClearsPrevMode(t *testing.T) {
	w := workerWithModes([]acp.SessionMode{{Id: "default", Name: "Default"}, {Id: "acceptEdits", Name: "Accept Edits"}}, "acceptEdits")
	workers := map[string]*AgentWorker{"claude": w}
	m := newTestModel(t, workers, policy.Routing{})
	m.autoPrevMode["claude"] = "default"

	updated, _ := m.Update(setModeResultMsg{agent: "claude", modeId: "default", modeLabel: "default", auto: true, turnOn: false})
	m = updated.(Model)

	if _, ok := m.autoPrevMode["claude"]; ok {
		t.Fatal("autoPrevMode[claude] still present after toggling auto off, want it cleared")
	}
	if out := m.viewport.View(); !strings.Contains(out, "auto mode off") {
		t.Fatalf("viewport.View() = %q, want an auto-mode-off confirmation", out)
	}
}

// TestModel_AutoCommand_TogglesOffOnSecondInvocation exercises the full
// toggle round trip through handleAutoCommand's own decision logic (not
// just setModeResultMsg's bookkeeping above): once autoPrevMode already
// has an entry for an agent, a second `auto <agent>` must target the
// remembered restore mode, not spec.AutoMode again.
func TestModel_AutoCommand_TogglesOffOnSecondInvocation(t *testing.T) {
	w := workerWithModes([]acp.SessionMode{{Id: "default", Name: "Default"}, {Id: "acceptEdits", Name: "Accept Edits"}}, "acceptEdits")
	workers := map[string]*AgentWorker{"claude": w}
	m := newTestModel(t, workers, policy.Routing{})
	m.agentSpecs = []session.Spec{{Name: "claude", AutoMode: "acceptEdits"}}
	m.autoPrevMode["claude"] = "default"

	_, cmd := enterWithInput(m, "auto claude")
	if cmd == nil {
		t.Fatal("enterWithInput(\"auto claude\") with an existing autoPrevMode entry returned a nil cmd, want the toggle-back runSetMode command")
	}
}

// --- Ctrl+C clear-then-quit --------------------------------------------

func TestModel_CtrlC_ClearsNonEmptyInputInsteadOfQuitting(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.input.SetValue("fix the login bug")

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(Model)

	if m.quitting {
		t.Fatal("Ctrl+C with non-empty input set quitting=true, want it to just clear the draft")
	}
	if cmd != nil {
		t.Fatal("Ctrl+C with non-empty input returned a non-nil cmd, want nil (no tea.Quit)")
	}
	if m.input.Value() != "" {
		t.Fatalf("input.Value() = %q after Ctrl+C, want cleared", m.input.Value())
	}
}

// TestModel_CtrlC_TakesInterruptBranchWhenBusy proves Ctrl+C checks
// busy-state BEFORE input content, the key behavioral change from the
// old clear-or-quit-only version. AgentWorker.Cancel needs a real
// session.AgentSession (which needs a live ACP connection) — untestable
// here the same way the rest of internal/session is (see
// TestModel_InterruptBusyAgents_NoOpWhenNothingIsBusy's own comment for
// the identical limitation on Esc). This test turns that limitation into
// a positive signal instead of working around it: newWorker() leaves
// sess nil, so recovering a panic FROM INSIDE AgentWorker.Cancel proves
// execution actually reached the interrupt branch — not the
// input-clearing branch, which would have returned cleanly with no
// panic at all. If a future change reorders these checks back to
// input-first, this test starts failing (no panic, because Cancel is
// never reached) instead of silently passing on the wrong branch.
func TestModel_CtrlC_TakesInterruptBranchWhenBusy(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want a panic from AgentWorker.Cancel's nil test session, proving the busy-interrupt branch was reached — got none, so Ctrl+C took some other branch (input-clearing?) instead of checking busy state first")
		}
	}()
	w := newWorker()
	w.busy.Store(true)
	m := newTestModel(t, map[string]*AgentWorker{"claude": w}, policy.Routing{})
	m.agentSpecs = []session.Spec{{Name: "claude"}}
	m.input.SetValue("half-typed prompt") // must be ignored — busy wins regardless
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
}

func TestModel_CtrlC_QuitsWhenInputAlreadyEmpty(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(Model)

	if !m.quitting {
		t.Fatal("Ctrl+C with empty input did not set quitting=true")
	}
	if cmd == nil {
		t.Fatal("Ctrl+C with empty input returned a nil cmd, want tea.Quit")
	}
}

// --- backslash-Enter newline continuation -------------------------------

func TestModel_BackslashEnter_InsertsNewlineInsteadOfSubmitting(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.input.SetValue(`claude: first line\`)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if cmd != nil {
		t.Fatal("backslash+Enter returned a non-nil cmd, want nil (must not submit/dispatch)")
	}
	want := "claude: first line\n"
	if got := m.input.Value(); got != want {
		t.Fatalf("input.Value() = %q after backslash+Enter, want %q", got, want)
	}
}

// --- turn-completion bell (bellSuffix) -----------------------------------

func TestModel_BellSuffix_SilentWhileFocused(t *testing.T) {
	m := newTestModel(t, nil, policy.Routing{})
	if got := m.bellSuffix(); got != "" {
		t.Fatalf("bellSuffix() = %q while focused, want \"\"", got)
	}
}

func TestModel_BellSuffix_RingsAfterBlur(t *testing.T) {
	m := newTestModel(t, nil, policy.Routing{})
	updated, _ := m.Update(tea.BlurMsg{})
	m = updated.(Model)
	if got := m.bellSuffix(); got != "\a" {
		t.Fatalf("bellSuffix() = %q after BlurMsg, want \"\\a\"", got)
	}

	updated, _ = m.Update(tea.FocusMsg{})
	m = updated.(Model)
	if got := m.bellSuffix(); got != "" {
		t.Fatalf("bellSuffix() = %q after a following FocusMsg, want \"\" (refocused)", got)
	}
}

func TestModel_PromptDoneMsg_AppendsBellWhenUnfocused(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	updated, _ := m.Update(tea.BlurMsg{})
	m = updated.(Model)

	updated, _ = m.Update(promptDoneMsg{e: PromptDoneMsg{Agent: "claude", Duration: 2 * time.Second}})
	m = updated.(Model)

	if out := m.viewport.View(); !strings.Contains(out, "\a") {
		t.Fatalf("viewport.View() after a completed turn while unfocused doesn't contain a bell char: %q", out)
	}
}

// --- large-paste collapse (storePasteIfLarge / expandPastes) ------------

func TestModel_StorePasteIfLarge_SmallPasteNotCollapsed(t *testing.T) {
	m := newTestModel(t, nil, policy.Routing{})
	if _, ok := m.storePasteIfLarge("just a short paste"); ok {
		t.Fatal("storePasteIfLarge collapsed a short, few-line paste, want it left alone")
	}
}

func TestModel_StorePasteIfLarge_CollapsesOverCharThreshold(t *testing.T) {
	m := newTestModel(t, nil, policy.Routing{})
	big := strings.Repeat("x", pasteCollapseCharThreshold+1)
	chip, ok := m.storePasteIfLarge(big)
	if !ok {
		t.Fatal("storePasteIfLarge did not collapse a paste over the char threshold")
	}
	if !strings.Contains(chip, "Pasted text #1") {
		t.Fatalf("chip = %q, want it to mention \"Pasted text #1\"", chip)
	}
	if got := m.pastes[chip]; got != big {
		t.Fatalf("m.pastes[chip] = %q (len %d), want the original paste back verbatim", got, len(got))
	}
}

func TestModel_StorePasteIfLarge_CollapsesOverLineThreshold(t *testing.T) {
	m := newTestModel(t, nil, policy.Routing{})
	big := "one\ntwo\nthree\nfour\nfive"
	if _, ok := m.storePasteIfLarge(big); !ok {
		t.Fatal("storePasteIfLarge did not collapse a paste over the line threshold despite being short")
	}
}

func TestModel_ExpandPastes_RoundTrips(t *testing.T) {
	m := newTestModel(t, nil, policy.Routing{})
	big := strings.Repeat("y", pasteCollapseCharThreshold+50)
	chip, ok := m.storePasteIfLarge(big)
	if !ok {
		t.Fatal("setup: expected paste to collapse")
	}
	line := "claude: what's wrong here? " + chip
	got := m.expandPastes(line)
	want := "claude: what's wrong here? " + big
	if got != want {
		t.Fatalf("expandPastes round-trip mismatch (got len %d, want len %d)", len(got), len(want))
	}
}

func TestModel_ExpandPastes_NoOpWithoutChips(t *testing.T) {
	m := newTestModel(t, nil, policy.Routing{})
	if got := m.expandPastes("plain text, nothing to expand"); got != "plain text, nothing to expand" {
		t.Fatalf("expandPastes changed plain text with no chips: %q", got)
	}
}

func TestModel_HandleKey_LargePasteInsertsChipNotRawText(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	big := strings.Repeat("z", pasteCollapseCharThreshold+1)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(big), Paste: true})
	m = updated.(Model)

	val := m.input.Value()
	if strings.Contains(val, big) {
		t.Fatal("input box contains the raw large paste, want it collapsed to a chip")
	}
	if !strings.Contains(val, "Pasted text #1") {
		t.Fatalf("input.Value() = %q, want it to contain the collapse chip", val)
	}
}

func TestModel_HandleKey_SmallPastePassesThroughUnchanged(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello"), Paste: true})
	m = updated.(Model)

	if got := m.input.Value(); got != "hello" {
		t.Fatalf("input.Value() = %q after a small paste, want \"hello\" inserted verbatim", got)
	}
}

func TestModel_DispatchUserTurn_ExpandsPasteChipBeforeSending(t *testing.T) {
	w := newWorker()
	m := newTestModel(t, map[string]*AgentWorker{"claude": w}, policy.Routing{})
	big := strings.Repeat("q", pasteCollapseCharThreshold+1)
	chip, ok := m.storePasteIfLarge(big)
	if !ok {
		t.Fatal("setup: expected paste to collapse")
	}

	m.dispatchUserTurn("claude", chip, true, true)

	select {
	case blocks := <-w.in:
		if len(blocks) != 1 || blocks[0].Text == nil || !strings.Contains(blocks[0].Text.Text, big) {
			t.Fatalf("queued content blocks = %+v, want the full expanded paste text", blocks)
		}
	default:
		t.Fatal("dispatchUserTurn did not queue anything to the worker")
	}

	if out := m.viewport.View(); strings.Contains(out, big) {
		t.Fatal("scrollback echo contains the full expanded paste, want it to keep showing the short chip")
	}
}

// --- live "/"-command and "@"-file suggestion popup (Model wiring) ------

func TestModel_TypingSlash_OpensCommandPopup(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "plan", Description: "make a plan"}}}
	m.input.SetValue("/pl")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = updated.(Model)

	if m.suggestKind != suggestCommand {
		t.Fatalf("suggestKind = %v after typing into a leading \"/\", want suggestCommand", m.suggestKind)
	}
	if len(m.suggestItems) != 1 || m.suggestItems[0].label != "/plan" {
		t.Fatalf("suggestItems = %+v, want [\"/plan\"]", m.suggestItems)
	}
	if !strings.Contains(m.renderSuggestOverlay(), "/plan") {
		t.Fatalf("renderSuggestOverlay() = %q, want it to mention /plan", m.renderSuggestOverlay())
	}
}

func TestModel_TabAcceptsHighlightedSuggestion(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "plan"}}}
	m.input.SetValue("/plan")
	m.refreshSuggest()

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)

	if got := m.input.Value(); got != "/plan " {
		t.Fatalf("input.Value() = %q after Tab-accepting the only match, want \"/plan \"", got)
	}
	if m.suggestKind != suggestNone {
		t.Fatal("suggestKind still active after accepting — want the popup to close since the token no longer matches")
	}
}

func TestModel_EnterAcceptsSuggestionInsteadOfSubmitting(t *testing.T) {
	w := newWorker()
	m := newTestModel(t, map[string]*AgentWorker{"claude": w}, policy.Routing{})
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "plan"}}}
	m.input.SetValue("/plan")
	m.refreshSuggest()

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if got := m.input.Value(); got != "/plan " {
		t.Fatalf("input.Value() = %q after Enter with a popup open, want it to accept (\"/plan \"), not submit", got)
	}
	select {
	case blocks := <-w.in:
		t.Fatalf("Enter with a suggestion popup open dispatched a prompt (%+v), want it to only accept the suggestion", blocks)
	default:
	}
}

func TestModel_ArrowKeysMoveSuggestCursor(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "plan"}, {Name: "playtest"}}}
	m.input.SetValue("/pla")
	m.refreshSuggest()
	if len(m.suggestItems) != 2 {
		t.Fatalf("setup: want 2 matches for \"/pla\", got %d", len(m.suggestItems))
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.suggestCursor != 1 {
		t.Fatalf("suggestCursor = %d after one Down, want 1", m.suggestCursor)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.suggestCursor != 0 {
		t.Fatalf("suggestCursor = %d after wrapping past the last item, want 0", m.suggestCursor)
	}
}

func TestModel_EscDismissesPopupInsteadOfInterrupting(t *testing.T) {
	w := newWorker()
	m := newTestModel(t, map[string]*AgentWorker{"claude": w}, policy.Routing{})
	m.agentSpecs = []session.Spec{{Name: "claude"}}
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "plan"}}}
	m.input.SetValue("/pla")
	m.refreshSuggest()

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.suggestKind != suggestNone {
		t.Fatal("suggestKind still active after Esc, want the popup dismissed")
	}
	// Typing further without changing the token (still "/pla") must keep
	// it dismissed until the token actually changes.
	m.refreshSuggest()
	if m.suggestKind != suggestNone {
		t.Fatal("popup reappeared on the same, still-suppressed token")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = updated.(Model)
	if m.suggestKind == suggestNone {
		t.Fatal("popup stayed dismissed after the token actually changed (\"/plan\"), want it to un-suppress")
	}
}

func TestModel_Relayout_ShrinksViewportWhilePopupOpen(t *testing.T) {
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.commands = map[string][]acp.AvailableCommand{"claude": {{Name: "plan"}}}
	before := m.viewport.Height

	m.input.SetValue("/pla")
	m.refreshSuggest()
	m.relayout()

	if m.viewport.Height >= before {
		t.Fatalf("viewport.Height = %d after opening a popup, want less than %d (the popup's lines subtracted)", m.viewport.Height, before)
	}
	wantOverlayLines := m.suggestOverlayLineCount()
	if before-m.viewport.Height != wantOverlayLines {
		t.Fatalf("viewport shrank by %d, want exactly suggestOverlayLineCount() = %d", before-m.viewport.Height, wantOverlayLines)
	}
}

// --- Ctrl+V clipboard image paste ---------------------------------------

// stubClipboardImage replaces readClipboardImage for the duration of a
// test — same pattern as stubClipboard (writeClipboard), so these tests
// never touch the real OS clipboard.
func stubClipboardImage(t *testing.T, data []byte, mime string, err error) {
	t.Helper()
	orig := readClipboardImage
	readClipboardImage = func() ([]byte, string, error) { return data, mime, err }
	t.Cleanup(func() { readClipboardImage = orig })
}

func TestModel_CtrlV_NoClipboardImageIsSilentNoOp(t *testing.T) {
	stubClipboardImage(t, nil, "", ErrNoClipboardImage)
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.input.SetValue("hello")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = updated.(Model)

	if got := m.input.Value(); got != "hello" {
		t.Fatalf("input.Value() = %q after Ctrl+V with no clipboard image, want it untouched", got)
	}
	if out := m.viewport.View(); strings.Contains(out, "failed") {
		t.Fatalf("viewport shows an error for the no-image case: %q", out)
	}
}

func TestModel_CtrlV_RealErrorAppendsLine(t *testing.T) {
	stubClipboardImage(t, nil, "", errors.New("boom"))
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = updated.(Model)

	if out := m.viewport.View(); !strings.Contains(out, "boom") {
		t.Fatalf("viewport.View() = %q, want the real clipboard-read error surfaced", out)
	}
}

func TestModel_CtrlV_SuccessSavesFileAndInsertsAtMention(t *testing.T) {
	fakePNG := []byte("fake-png-bytes")
	stubClipboardImage(t, fakePNG, "image/png", nil)
	m := newTestModel(t, map[string]*AgentWorker{"claude": newWorker()}, policy.Routing{})
	m.imageDir = t.TempDir()
	m.input.SetValue("claude: what is this? ")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = updated.(Model)

	val := m.input.Value()
	if !strings.Contains(val, "@"+m.imageDir) || !strings.HasSuffix(strings.TrimSpace(val), ".png") {
		t.Fatalf("input.Value() = %q, want an @-mention pointing at a .png file under %q", val, m.imageDir)
	}
	entries, err := os.ReadDir(m.imageDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("os.ReadDir(imageDir) = %v, %v, want exactly one saved file", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(m.imageDir, entries[0].Name()))
	if err != nil || string(data) != string(fakePNG) {
		t.Fatalf("saved file content = %q, %v, want the clipboard bytes verbatim", data, err)
	}
}
