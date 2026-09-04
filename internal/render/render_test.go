package render

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/glamour"
	acp "github.com/coder/acp-go-sdk"

	"chorus/internal/bus"
)

func newTestRenderer() *Renderer {
	return New()
}

func update(agent string, sessionID acp.SessionId, u acp.SessionUpdate) bus.Update {
	return bus.Update{
		Agent: agent,
		Notification: acp.SessionNotification{
			SessionId: sessionID,
			Update:    u,
		},
	}
}

func strPtr(s string) *string                            { return &s }
func statusPtr(s acp.ToolCallStatus) *acp.ToolCallStatus { return &s }

func TestFormatUpdate_AgentThoughtChunk_ShowsWhenEnabled(t *testing.T) {
	r := newTestRenderer()
	r.ShowThoughts = true
	_, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("pondering")},
	}))
	if !ok {
		t.Fatal("ok = false, want true when ShowThoughts is enabled")
	}
	if !strings.Contains(text, "thinking") || !strings.Contains(text, "pondering") {
		t.Fatalf("text = %q, want (thinking) + text", text)
	}
}

func TestFormatUpdate_ToolCallUpdate_ReplacesTitleWhenPresent(t *testing.T) {
	r := newTestRenderer()
	r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tc1", Title: "old title", Status: acp.ToolCallStatusPending},
	}))
	_, text, _ := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		ToolCallUpdate: &acp.SessionToolCallUpdate{
			ToolCallId: "tc1",
			Title:      strPtr("new title"),
			Status:     statusPtr(acp.ToolCallStatusInProgress),
		},
	}))
	if !strings.Contains(text, "new title") {
		t.Fatalf("text = %q, want the updated title", text)
	}
}

func TestFormatUpdate_Plan_Checklist(t *testing.T) {
	r := newTestRenderer()
	_, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		Plan: &acp.SessionUpdatePlan{
			Entries: []acp.PlanEntry{
				{Content: "step one", Status: acp.PlanEntryStatusCompleted},
				{Content: "step two", Status: acp.PlanEntryStatusInProgress},
				{Content: "step three", Status: acp.PlanEntryStatusPending},
			},
		},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	for _, want := range []string{"step one", "step two", "step three", "[x]", "[~]", "[ ]"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q; got %q", want, text)
		}
	}
}

func TestFormatUpdate_UsageUpdate_QuietNotRawJSON(t *testing.T) {
	r := newTestRenderer()
	_, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		UsageUpdate: &acp.SessionUsageUpdate{Used: 100, Size: 200000},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !strings.Contains(text, "100") || !strings.Contains(text, "200000") {
		t.Fatalf("text = %q, want token counts shown", text)
	}
	if strings.Contains(text, "unrecognized") {
		t.Fatalf("text = %q, usage_update should never hit the unrecognized-kind fallback", text)
	}
}

func TestFormatUpdate_SessionInfoUpdate_ShowsTitle(t *testing.T) {
	r := newTestRenderer()
	_, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		SessionInfoUpdate: &acp.SessionSessionInfoUpdate{Title: strPtr("Fix the login bug")},
	}))
	if !ok {
		t.Fatal("ok = false, want true when a title is present")
	}
	if !strings.Contains(text, "Fix the login bug") {
		t.Fatalf("text = %q, want the session title shown", text)
	}
}

func TestFormatPermissionPrompt_ShowsWhateverOptionsAreOffered(t *testing.T) {
	r := newTestRenderer()
	out := r.FormatPermissionPrompt("claude", acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{Title: strPtr("Write auth.py")},
		Options: []acp.PermissionOption{
			{OptionId: "1", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
			{OptionId: "2", Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
		},
	}, 0)
	for _, want := range []string{"Write auth.py", "Deny", "Allow Once", "reject_once", "allow_once"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q; got %q", want, out)
		}
	}
}

func TestFormatPermissionPrompt_CursorMarksSelectedOption(t *testing.T) {
	r := newTestRenderer()
	req := acp.RequestPermissionRequest{
		Options: []acp.PermissionOption{
			{OptionId: "1", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
			{OptionId: "2", Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
		},
	}
	at0 := r.FormatPermissionPrompt("claude", req, 0)
	at1 := r.FormatPermissionPrompt("claude", req, 1)
	if !strings.Contains(at0, "❯") {
		t.Errorf("cursor at 0 = %q, want a cursor marker present", at0)
	}
	if at0 == at1 {
		t.Fatal("FormatPermissionPrompt() at cursor 0 and cursor 1 produced identical text — the cursor must visibly move")
	}
}

func TestFormatMenuLine_MarksOnlyTheCursorRow(t *testing.T) {
	other := FormatMenuLine(0, 1, "not selected")
	selected := FormatMenuLine(1, 1, "selected")
	if strings.Contains(other, "❯") {
		t.Errorf("FormatMenuLine(0, cursor=1, ...) = %q, want no cursor marker on a non-selected row", other)
	}
	if !strings.Contains(selected, "❯") {
		t.Errorf("FormatMenuLine(1, cursor=1, ...) = %q, want a cursor marker on the selected row", selected)
	}
}

func TestFormatUserPrompt_HighlightsFullWidth(t *testing.T) {
	r := newTestRenderer()
	r.SetWidth(40)
	out := r.FormatUserPrompt("claude", "fix the bug")

	if !strings.Contains(out, "claude") {
		t.Errorf("text = %q, want the agent tag present", out)
	}
	if !strings.Contains(out, "fix the bug") {
		t.Errorf("text = %q, want the prompt text present", out)
	}
	if !strings.Contains(out, "\x1b[100m") {
		t.Errorf("text = %q, want the background-highlight escape present", out)
	}
	// The highlighted line (agent tag + "\n" + highlighted content + "\n")
	// should pad the content out to the full configured width, not just
	// wrap the bare text — strip the tag line and the trailing newline to
	// isolate it.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("FormatUserPrompt() produced %d lines, want 2 (agent tag, then the highlighted content)", len(lines))
	}
	visible := StripANSI(lines[1])
	if len([]rune(visible)) != 40 {
		t.Errorf("highlighted line visible width = %d, want exactly 40 (r.width)", len([]rune(visible)))
	}
}

func TestFormatUserPrompt_EmptyTextReturnsEmpty(t *testing.T) {
	r := newTestRenderer()
	if out := r.FormatUserPrompt("claude", ""); out != "" {
		t.Fatalf("FormatUserPrompt(agent, \"\") = %q, want empty", out)
	}
}

func TestFormatUserPrompt_MultilinePromptHighlightsEachLine(t *testing.T) {
	r := newTestRenderer()
	r.SetWidth(30)
	out := r.FormatUserPrompt("claude", "line one\nline two")
	if strings.Count(out, "\x1b[100m") != 2 {
		t.Errorf("text = %q, want a highlight escape on each of the 2 lines", out)
	}
}

// --- diff rendering (renderDiff is a free function — no Renderer needed) ---

func TestRenderDiff_ShowsAddedAndRemovedLines(t *testing.T) {
	out := renderDiff(acp.ToolCallContentDiff{
		Path:    "auth.py",
		OldText: strPtr("line1\nline2\nline3"),
		NewText: "line1\nCHANGED\nline3",
	})
	if !strings.Contains(out, "auth.py") {
		t.Errorf("diff missing path; got %q", out)
	}
	if !strings.Contains(out, "-line2") {
		t.Errorf("diff missing removed line; got %q", out)
	}
	if !strings.Contains(out, "+CHANGED") {
		t.Errorf("diff missing added line; got %q", out)
	}
	if !strings.Contains(out, "@@") {
		t.Errorf("diff missing hunk header; got %q", out)
	}
}

func TestRenderDiff_NewFileHasNoOldText(t *testing.T) {
	out := renderDiff(acp.ToolCallContentDiff{
		Path:    "new.txt",
		OldText: nil,
		NewText: "brand new content",
	})
	if !strings.Contains(out, "+brand new content") {
		t.Errorf("diff missing added content for a new file; got %q", out)
	}
}

func TestFormatUpdate_ToolCall_DiffContentRenders(t *testing.T) {
	r := newTestRenderer()
	_, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{
			ToolCallId: "tc1",
			Title:      "Edit auth.py",
			Status:     acp.ToolCallStatusCompleted,
			Content: []acp.ToolCallContent{
				{Diff: &acp.ToolCallContentDiff{Path: "auth.py", OldText: strPtr("a"), NewText: "b"}},
			},
		},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !strings.Contains(text, "auth.py") || !strings.Contains(text, "-a") || !strings.Contains(text, "+b") {
		t.Fatalf("tool call text missing rendered diff; got %q", text)
	}
}

func TestFormatUpdate_AvailableCommandsUpdate_IsTerseNotAFullList(t *testing.T) {
	r := newTestRenderer()
	_, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
			AvailableCommands: []acp.AvailableCommand{
				{Name: "design-sync", Description: "sync design"},
				{Name: "review", Description: "review changes"},
			},
		},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if strings.Contains(text, "design-sync") || strings.Contains(text, "review") {
		t.Fatalf("text = %q, want a terse message, not the full command list (found live to be noisy with a large command set)", text)
	}
	if !strings.Contains(text, "commands") {
		t.Fatalf("text = %q, want it to at least point at the \"commands\" REPL command", text)
	}
}

func TestFormatUpdate_ToolCall_LargeTextContentIsTruncated(t *testing.T) {
	r := newTestRenderer()
	big := strings.Repeat("x", toolCallContentPreviewLimit+1000)
	_, text, ok := r.FormatUpdate(update("opencode", "s1", acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{
			ToolCallId: "tc1",
			Title:      "Read chorus-spec.md",
			Status:     acp.ToolCallStatusCompleted,
			Content: []acp.ToolCallContent{
				{Content: &acp.ToolCallContentContent{Content: acp.TextBlock(big)}},
			},
		},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if strings.Count(text, "x") > toolCallContentPreviewLimit {
		t.Fatalf("tool call text contains more than %d of the repeated character, want it capped (found live: an unbounded file-read dump flooding the transcript)", toolCallContentPreviewLimit)
	}
	if !strings.Contains(text, "more chars") {
		t.Fatalf("text = %q, want a truncation note", text)
	}
}

func TestFormatUpdate_ToolCall_ShortTextContentIsNotTruncated(t *testing.T) {
	r := newTestRenderer()
	_, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{
			ToolCallId: "tc1",
			Title:      "Read auth.py",
			Status:     acp.ToolCallStatusCompleted,
			Content: []acp.ToolCallContent{
				{Content: &acp.ToolCallContentContent{Content: acp.TextBlock("short content")}},
			},
		},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !strings.Contains(text, "short content") {
		t.Fatalf("text = %q, want short content shown in full, untruncated", text)
	}
	if strings.Contains(text, "more chars") {
		t.Fatalf("text = %q, want no truncation note for content under the limit", text)
	}
}

func TestFormatUpdate_Image_NoImageDirFallsBackToPlaceholder(t *testing.T) {
	r := newTestRenderer() // WithImageDir never called
	_, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			Content: acp.ImageBlock(base64.StdEncoding.EncodeToString([]byte("fake-png-bytes")), "image/png"),
		},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !strings.Contains(text, "[image]") {
		t.Fatalf("text = %q, want the [image] placeholder when no image dir is configured", text)
	}
}

func TestFormatUpdate_Image_SavesToConfiguredDir(t *testing.T) {
	dir := t.TempDir()
	r := newTestRenderer()
	r.WithImageDir(dir)

	want := []byte("fake-png-bytes")
	_, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			Content: acp.ImageBlock(base64.StdEncoding.EncodeToString(want), "image/png"),
		},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !strings.Contains(text, dir) || !strings.Contains(text, ".png") {
		t.Fatalf("text = %q, want a saved-image path under %q ending in .png", text, dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want exactly 1 saved image file", len(entries))
	}
	got, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("saved file contents = %q, want %q", got, want)
	}
}

func TestFormatUpdate_Image_SequentialFilesDontCollide(t *testing.T) {
	dir := t.TempDir()
	r := newTestRenderer()
	r.WithImageDir(dir)

	for i := 0; i < 3; i++ {
		r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ImageBlock(base64.StdEncoding.EncodeToString([]byte("img")), "image/png"),
			},
		}))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3 distinct saved files", len(entries))
	}
}

func TestStripANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[2K\x1b[1Aoverwrite", "overwrite"},
		{"before\x1b]0;evil title\x07after", "beforeafter"},
		{"", ""},
	}
	for _, c := range cases {
		if got := StripANSI(c.in); got != c.want {
			t.Errorf("StripANSI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Regression test for chorus-spec.md §0's 2026-08-22 audit finding:
// agent-supplied text was interpolated into terminal output with no
// escape-sequence sanitization, so a malicious or manipulated agent
// response could embed control sequences to hide or spoof what's shown.
func TestFormatUpdate_AgentMessageChunk_StripsEmbeddedANSI(t *testing.T) {
	r := newTestRenderer()
	_, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			Content: acp.TextBlock("safe\x1b[2K\x1b[1Ainjected"),
		},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if strings.Contains(text, "\x1b[2K") {
		t.Fatalf("text = %q, want the embedded escape sequence stripped", text)
	}
	if !strings.Contains(text, "safeinjected") {
		t.Fatalf("text = %q, want the surrounding text preserved with the escape removed", text)
	}
}

// --- streaming markdown text -------------------------------------------

func TestFormatUpdate_StreamingText_AccumulatesAcrossChunks(t *testing.T) {
	r := newTestRenderer()
	r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("hello ")},
	}))
	_, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("world")},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !strings.Contains(text, "hello") || !strings.Contains(text, "world") {
		t.Fatalf("text = %q, want both chunks present (whole-buffer re-render)", text)
	}
}

func TestFormatUpdate_StreamingText_ThoughtAndMessageDontShareABuffer(t *testing.T) {
	r := newTestRenderer()
	r.ShowThoughts = true // exercise the full-text stream path, not the indicator
	_, thoughtText, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("pondering the question")},
	}))
	if !ok || !strings.Contains(thoughtText, "pondering the question") {
		t.Fatalf("thought text = %q (ok=%v), want the thought rendered", thoughtText, ok)
	}
	_, messageText, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("the answer")},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !strings.Contains(messageText, "the answer") {
		t.Fatalf("message text = %q, want the message text present", messageText)
	}
	if strings.Contains(messageText, "pondering the question") {
		t.Fatalf("message text = %q, want it NOT to contain the separate thought stream's text — thought and message must not share a buffer", messageText)
	}
}

func TestRenderMarkdown_BoldTextIsStyled(t *testing.T) {
	// newTestRenderer's glamour instance uses WithAutoStyle(), which
	// (correctly) detects `go test`'s non-TTY stdout and falls back to
	// no styling — the same fallback a real user piping chorus's output
	// to a file would get. That's real-world-correct behavior, not
	// something this test should fight. To verify the transformation
	// logic itself deterministically, construct glamour directly with
	// an explicit style, bypassing New()'s auto-detection.
	md, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(80))
	if err != nil {
		t.Fatalf("glamour.NewTermRenderer() error = %v", err)
	}
	r := &Renderer{md: md}
	out := r.renderMarkdown("**bold**")
	if strings.Contains(out, "**bold**") {
		t.Fatalf("renderMarkdown(%q) = %q, want the raw asterisks transformed into ANSI styling, not left literal", "**bold**", out)
	}
	if !strings.Contains(out, "bold") {
		t.Fatalf("renderMarkdown() = %q, want the word itself preserved", out)
	}
}

func TestRenderMarkdown_NilRendererFallsBackToPlainText(t *testing.T) {
	r := &Renderer{} // md is nil — glamour never constructed
	got := r.renderMarkdown("**bold**")
	if got != "**bold**" {
		t.Fatalf("renderMarkdown() = %q, want the raw text returned unchanged when no renderer is available", got)
	}
}

// --- FormatUpdate/FormatSpinnerTick pure core ---------------------------
//
// The pure (key, text, ok) surface a bubbletea View() calls directly —
// see internal/tui. No io.Writer, no ANSI cursor-redraw involved.

func TestFormatUpdate_AgentMessageChunk_ReturnsTextNoKey(t *testing.T) {
	r := newTestRenderer()
	key, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("hello there")},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if key != "stream|claude|message" {
		t.Errorf("key = %q, want the stream|agent|kind merge key", key)
	}
	if !strings.Contains(text, "claude") || !strings.Contains(text, "hello there") {
		t.Errorf("text = %q, want it to contain agent tag and text", text)
	}
}

func TestFormatUpdate_ToolCall_KeyIncludesAgentAndID(t *testing.T) {
	r := newTestRenderer()
	key, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tc1", Title: "Read auth.py", Status: acp.ToolCallStatusPending},
	}))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if key != "tool|claude|tc1" {
		t.Errorf("key = %q, want tool|claude|tc1", key)
	}
	if !strings.Contains(text, "Read auth.py") {
		t.Errorf("text = %q, want the title", text)
	}
}

func TestFormatUpdate_ThoughtChunk_ShowsIndicatorWhenShowThoughtsFalse(t *testing.T) {
	r := newTestRenderer()
	r.ShowThoughts = false
	key, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("the real reasoning text")},
	}))
	if !ok {
		t.Fatal("ok = false, want true — a brief indicator should show even with thoughts hidden")
	}
	if key != "stream|claude|thought" {
		t.Errorf("key = %q, want stream|claude|thought (mergeable, so repeated chunks don't spam lines)", key)
	}
	if !strings.Contains(text, "claude") {
		t.Errorf("text = %q, want the agent tag present", text)
	}
	if strings.Contains(text, "the real reasoning text") {
		t.Fatalf("text = %q, want the actual thought content NOT leaked when ShowThoughts is false", text)
	}
	found := false
	for _, w := range thinkingWords {
		if strings.Contains(text, w) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("text = %q, want one of the thinkingWords present", text)
	}
}

func TestFormatUpdate_ThinkingIndicator_WordStaysStableAcrossChunks(t *testing.T) {
	r := newTestRenderer()
	r.ShowThoughts = false
	_, first, _ := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("chunk one")},
	}))
	_, second, _ := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("chunk two")},
	}))
	wordOf := func(s string) string {
		for _, w := range thinkingWords {
			if strings.Contains(s, w) {
				return w
			}
		}
		return ""
	}
	w1, w2 := wordOf(first), wordOf(second)
	if w1 == "" || w2 == "" {
		t.Fatalf("couldn't find a thinkingWord in %q / %q", first, second)
	}
	if w1 != w2 {
		t.Errorf("word changed mid-burst: %q then %q, want it fixed for the burst's duration", w1, w2)
	}
}

func TestFormatUpdate_ThinkingIndicator_ClearedWhenMessageStarts(t *testing.T) {
	r := newTestRenderer()
	r.ShowThoughts = false
	r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("thinking...")},
	}))
	if _, _, ok := r.FormatSpinnerTick(); !ok {
		t.Fatal("FormatSpinnerTick() ok = false, want true while a thinking burst is active")
	}
	r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("the actual reply")},
	}))
	if _, _, ok := r.FormatSpinnerTick(); ok {
		t.Fatal("FormatSpinnerTick() ok = true, want false — the thinking indicator should be cleared once the message stream starts")
	}
}

func TestFormatSpinnerTick_AnimatesThinkingIndicator(t *testing.T) {
	r := newTestRenderer()
	r.ShowThoughts = false
	r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("thinking...")},
	}))
	key, text, ok := r.FormatSpinnerTick()
	if !ok {
		t.Fatal("ok = false, want true while a thinking burst is active")
	}
	if key != "stream|claude|thought" {
		t.Errorf("key = %q, want stream|claude|thought", key)
	}
	if !strings.Contains(text, "claude") {
		t.Errorf("text = %q, want the agent tag present", text)
	}
}

func TestFormatSpinnerTick_ToolCallTakesPriorityOverThinkingIndicator(t *testing.T) {
	r := newTestRenderer()
	r.ShowThoughts = false
	r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tc1", Title: "Running", Status: acp.ToolCallStatusInProgress},
	}))
	key, _, ok := r.FormatSpinnerTick()
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if key != "tool|claude|tc1" {
		t.Errorf("key = %q, want the tool call's key — a thinking indicator must already have been cleared by the tool call starting", key)
	}
}

func TestFormatUpdate_SessionInfoUpdate_NoTitleNotOk(t *testing.T) {
	r := newTestRenderer()
	_, _, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		SessionInfoUpdate: &acp.SessionSessionInfoUpdate{UpdatedAt: strPtr("2026-01-01T00:00:00Z")},
	}))
	if ok {
		t.Fatal("ok = true, want false for a title-less session_info_update")
	}
}

func TestFormatUpdate_UnrecognizedKind_StillOkAndTagged(t *testing.T) {
	r := newTestRenderer()
	key, text, ok := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{}))
	if !ok {
		t.Fatal("ok = false, want true — an unrecognized update kind must still render, tagged")
	}
	if key != "" {
		t.Errorf("key = %q, want empty (one-shot, non-mergeable)", key)
	}
	if !strings.Contains(text, "unrecognized") {
		t.Errorf("text = %q, want it tagged unrecognized", text)
	}
}

func TestFormatUpdate_ToolCallUpdate_PreservesTitleWhenOmitted(t *testing.T) {
	r := newTestRenderer()
	r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tc1", Title: "Read auth.py", Status: acp.ToolCallStatusPending},
	}))
	_, text, _ := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "tc1", Status: statusPtr(acp.ToolCallStatusCompleted)},
	}))
	if !strings.Contains(text, "Read auth.py") {
		t.Fatalf("text = %q, want the original title preserved across a titleless update", text)
	}
}

func TestFormatUpdate_StreamInterruptedByToolCall_StartsFreshNextTime(t *testing.T) {
	r := newTestRenderer()
	r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("thinking out loud")},
	}))
	r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tc1", Title: "Read x", Status: acp.ToolCallStatusCompleted},
	}))
	_, text, _ := r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("new reply")},
	}))
	if strings.Contains(text, "thinking out loud") {
		t.Fatalf("text = %q, want the interrupted stream's old text NOT reappearing (buffer should have reset)", text)
	}
	if !strings.Contains(text, "new reply") {
		t.Fatalf("text = %q, want the new message text present", text)
	}
}

func TestFormatSpinnerTick_NotOkWhenNothingInProgress(t *testing.T) {
	r := newTestRenderer()
	_, _, ok := r.FormatSpinnerTick()
	if ok {
		t.Fatal("ok = true, want false when no tool call is in progress")
	}
}

func TestFormatSpinnerTick_ReturnsSameKeyAsTheInProgressToolCall(t *testing.T) {
	r := newTestRenderer()
	r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tc1", Title: "Running", Status: acp.ToolCallStatusInProgress},
	}))
	key, text, ok := r.FormatSpinnerTick()
	if !ok {
		t.Fatal("ok = false, want true while a tool call is in_progress")
	}
	if key != "tool|claude|tc1" {
		t.Errorf("key = %q, want tool|claude|tc1", key)
	}
	if !strings.Contains(text, "Running") {
		t.Errorf("text = %q, want the tool call title", text)
	}
}

func TestFormatSpinnerTick_StopsAfterToolCallCompletes(t *testing.T) {
	r := newTestRenderer()
	r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tc1", Title: "Running", Status: acp.ToolCallStatusInProgress},
	}))
	r.FormatUpdate(update("claude", "s1", acp.SessionUpdate{
		ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "tc1", Status: statusPtr(acp.ToolCallStatusCompleted)},
	}))
	if _, _, ok := r.FormatSpinnerTick(); ok {
		t.Fatal("FormatSpinnerTick() ok = true, want false once the tool call has completed")
	}
}

func TestSetWidth_RebuildsMarkdownRendererWithoutPanicking(t *testing.T) {
	r := newTestRenderer()
	r.SetStyle(glamour.WithStandardStyle("dark"))
	r.SetWidth(40)
	out := r.renderMarkdown("**bold**")
	if strings.Contains(out, "**bold**") {
		t.Fatalf("renderMarkdown(%q) = %q, want styled output after SetWidth/SetStyle", "**bold**", out)
	}
}

func TestSetStyle_ForcesDeterministicStylingRegardlessOfTTY(t *testing.T) {
	// Unlike newTestRenderer()'s default (glamour.WithAutoStyle(), which
	// disables styling under go test's non-TTY buffer), SetStyle forces a
	// specific style so this is deterministic without bypassing New().
	r := newTestRenderer()
	r.SetStyle(glamour.WithStandardStyle("dark"))
	out := r.renderMarkdown("**bold**")
	if strings.Contains(out, "**bold**") {
		t.Fatalf("renderMarkdown(%q) = %q, want the raw asterisks transformed into ANSI styling", "**bold**", out)
	}
	if !strings.Contains(out, "bold") {
		t.Fatalf("renderMarkdown() = %q, want the word itself preserved", out)
	}
}

// --- native ("!") command formatting -----------------------------------

func TestFormatNativeCommandEcho_ShowsCommandOnHighlightedLine(t *testing.T) {
	r := newTestRenderer()
	r.SetWidth(40)
	out := r.FormatNativeCommandEcho("go test ./...")
	if !strings.Contains(out, "go test ./...") {
		t.Errorf("text = %q, want the command line present", out)
	}
	if !strings.Contains(out, "\x1b[100m") {
		t.Errorf("text = %q, want the background-highlight escape present, same convention as FormatUserPrompt", out)
	}
	if !strings.Contains(out, "[!]") {
		t.Errorf("text = %q, want the native-command tag [!]", out)
	}
}

func TestFormatNativeCommandEcho_EmptyReturnsEmpty(t *testing.T) {
	r := newTestRenderer()
	if out := r.FormatNativeCommandEcho(""); out != "" {
		t.Fatalf("FormatNativeCommandEcho(\"\") = %q, want empty", out)
	}
}

func TestFormatNativeCommandResult_SuccessShowsOutputAndDuration(t *testing.T) {
	out := FormatNativeCommandResult("echo hi", "hi\n", nil, 250*time.Millisecond)
	if !strings.Contains(out, "hi") {
		t.Errorf("text = %q, want the command's output present", out)
	}
	if !strings.Contains(out, "done in 250ms") {
		t.Errorf("text = %q, want a success status line with duration", out)
	}
	if strings.Contains(out, "failed") {
		t.Errorf("text = %q, want no failure wording on a nil error", out)
	}
}

func TestFormatNativeCommandResult_FailureShowsError(t *testing.T) {
	out := FormatNativeCommandResult("false", "", errors.New("exit status 1"), 10*time.Millisecond)
	if !strings.Contains(out, "failed") || !strings.Contains(out, "exit status 1") {
		t.Errorf("text = %q, want a failure status line including the error", out)
	}
}

func TestFormatNativeCommandResult_StripsEmbeddedANSI(t *testing.T) {
	out := FormatNativeCommandResult("cmd", "before\x1b[31mred\x1b[0mafter", nil, time.Second)
	if strings.Contains(out, "\x1b[31m") {
		t.Errorf("text = %q, want output ANSI stripped, same as agent-supplied text (security invariant #3)", out)
	}
	if !strings.Contains(out, "beforeredafter") {
		t.Errorf("text = %q, want the underlying text preserved after stripping", out)
	}
}

func TestFormatNativeCommandResult_TruncatesOversizedOutput(t *testing.T) {
	huge := strings.Repeat("a", nativeCommandOutputLimit+1000)
	out := FormatNativeCommandResult("cmd", huge, nil, time.Second)
	if !strings.Contains(out, "truncated") {
		t.Fatalf("text missing 'truncated' marker for output over the %d-byte limit", nativeCommandOutputLimit)
	}
	if strings.Count(out, "a") > nativeCommandOutputLimit+100 {
		t.Fatalf("output not actually capped near the %d-byte limit", nativeCommandOutputLimit)
	}
}
