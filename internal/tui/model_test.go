package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	acp "github.com/coder/acp-go-sdk"

	"chorus/internal/bus"
	"chorus/internal/delegate"
	"chorus/internal/policy"
	"chorus/internal/render"
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
	m := newTestModel(t, workers, policy.Routing{AskWhenAmbiguous: true})

	// An unmatched prompt with ask_when_ambiguous triggers a routeAsk.
	m, _ = enterWithInput(m, "some ambiguous prompt")
	if m.pendingRoute == nil {
		t.Fatal("pendingRoute = nil, want a pending route after an ambiguous prompt")
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
		if len(blocks) != 1 || blocks[0].Text == nil || blocks[0].Text.Text != "some ambiguous prompt" {
			t.Fatalf("claude's queued blocks = %+v, want the original ambiguous prompt text", blocks)
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
