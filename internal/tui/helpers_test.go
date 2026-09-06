package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"chorus/internal/policy"
	"chorus/internal/session"
)

// --- AgentWorker idle tracking (CLAUDE.md's delegation-maximization plan
// item 2 — acpclient.Client's nudge logic reads this via
// session.Connection.SetIdleChecker) ----------------------------------

func TestAgentWorker_IdleReflectsBusyState(t *testing.T) {
	w := newWorker()
	if !w.Idle() {
		t.Fatal("Idle() = false, want true for a freshly constructed worker with no prompt in flight")
	}
	w.busy.Store(true)
	if w.Idle() {
		t.Fatal("Idle() = true, want false once busy is set (mirrors StartWorker's goroutine while PromptContent is in flight)")
	}
	w.busy.Store(false)
	if !w.Idle() {
		t.Fatal("Idle() = false, want true again once busy clears")
	}
}

// TestAgentWorker_StartedAt covers the field the "<agent> working (...)"
// status line (Model.formatBusyStatus) reads every render — added
// alongside busy tracking so a long-running turn with no streamed output
// yet doesn't look identical to nothing happening at all.
func TestAgentWorker_StartedAt(t *testing.T) {
	w := newWorker()
	if _, ok := w.StartedAt(); ok {
		t.Fatal("StartedAt() ok = true for an idle worker that never started a prompt, want false")
	}

	want := time.Now().Add(-5 * time.Second)
	w.startedAt.Store(want)
	w.busy.Store(true)
	got, ok := w.StartedAt()
	if !ok || !got.Equal(want) {
		t.Fatalf("StartedAt() = (%v, %v), want (%v, true) while busy", got, ok, want)
	}

	w.busy.Store(false)
	if _, ok := w.StartedAt(); ok {
		t.Fatal("StartedAt() ok = true after busy cleared, want false — StartedAt must not be trusted once idle")
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{59500 * time.Millisecond, "1m00s"}, // rounds up across the minute boundary
		{2*time.Minute + 14*time.Second, "2m14s"},
		{time.Hour + 5*time.Minute, "1h05m"},
	}
	for _, c := range cases {
		if got := formatDuration(c.d); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// --- slash-command lookup -------------------------------------------

func TestCommandName(t *testing.T) {
	cases := map[string]string{
		"/compact":          "compact",
		"/compact do it":    "compact",
		"/code-review file": "code-review",
		"not-a-command":     "not-a-command", // no leading slash trimmed, but still "the first word"
	}
	for line, want := range cases {
		if got := commandName(line); got != want {
			t.Errorf("commandName(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestCommandOwners_SingleOwner(t *testing.T) {
	commands := map[string][]acp.AvailableCommand{
		"claude":   {{Name: "compact"}},
		"opencode": {{Name: "init"}},
	}
	owners := commandOwners(commands, "/compact do it")
	if len(owners) != 1 || owners[0] != "claude" {
		t.Fatalf("commandOwners() = %v, want [claude]", owners)
	}
}

func TestCommandOwners_CaseInsensitive(t *testing.T) {
	commands := map[string][]acp.AvailableCommand{
		"claude": {{Name: "Compact"}},
	}
	owners := commandOwners(commands, "/compact")
	if len(owners) != 1 || owners[0] != "claude" {
		t.Fatalf("commandOwners() = %v, want [claude]", owners)
	}
}

func TestCommandOwners_MultipleOwnersSorted(t *testing.T) {
	commands := map[string][]acp.AvailableCommand{
		"opencode": {{Name: "review"}},
		"claude":   {{Name: "review"}},
	}
	owners := commandOwners(commands, "/review")
	if len(owners) != 2 || owners[0] != "claude" || owners[1] != "opencode" {
		t.Fatalf("commandOwners() = %v, want [claude opencode] (sorted)", owners)
	}
}

func TestCommandOwners_NoOwner(t *testing.T) {
	commands := map[string][]acp.AvailableCommand{
		"claude": {{Name: "compact"}},
	}
	if owners := commandOwners(commands, "/nonexistent"); owners != nil {
		t.Fatalf("commandOwners() = %v, want nil", owners)
	}
}

// --- findCommandByAlias (auto-compaction / model-switch discovery) -----

func TestFindCommandByAlias_MatchesSubstringCaseInsensitive(t *testing.T) {
	cmds := []acp.AvailableCommand{{Name: "init"}, {Name: "Compact-History"}}
	got, ok := findCommandByAlias(cmds, []string{"compact", "summarize", "condense"})
	if !ok || got.Name != "Compact-History" {
		t.Fatalf("findCommandByAlias() = (%+v, %v), want Compact-History matched via substring", got, ok)
	}
}

func TestFindCommandByAlias_NoMatch(t *testing.T) {
	cmds := []acp.AvailableCommand{{Name: "init"}, {Name: "review"}}
	_, ok := findCommandByAlias(cmds, []string{"compact", "summarize", "condense"})
	if ok {
		t.Fatal("findCommandByAlias() ok = true, want false — none of the commands match any alias")
	}
}

func TestFindCommandByAlias_EmptyCommandsOrAliases(t *testing.T) {
	if _, ok := findCommandByAlias(nil, []string{"compact"}); ok {
		t.Fatal("findCommandByAlias(nil, ...) ok = true, want false")
	}
	if _, ok := findCommandByAlias([]acp.AvailableCommand{{Name: "compact"}}, nil); ok {
		t.Fatal("findCommandByAlias(..., nil) ok = true, want false")
	}
}

func TestFormatCommands_ListsPerAgentSorted(t *testing.T) {
	commands := map[string][]acp.AvailableCommand{
		"claude": {
			{Name: "zzz", Description: "last"},
			{Name: "aaa", Description: "first"},
		},
	}
	out := formatCommands(commands)
	iAAA := indexOf(out, "/aaa")
	iZZZ := indexOf(out, "/zzz")
	if iAAA == -1 || iZZZ == -1 || iAAA > iZZZ {
		t.Fatalf("formatCommands() didn't list commands sorted within an agent; got %q", out)
	}
}

func TestFormatCommands_EmptyIsFriendly(t *testing.T) {
	out := formatCommands(map[string][]acp.AvailableCommand{})
	if out == "" {
		t.Fatal("formatCommands() on an empty map returned empty string, want a friendly message")
	}
}

// --- stats (CLAUDE.md's delegation-maximization plan item 4) ---------

func TestFormatStats_NoActivityIsFriendly(t *testing.T) {
	specs := []session.Spec{{Name: "claude"}, {Name: "opencode"}}
	out := formatStats(specs, newAgentStats())
	if out == "" {
		t.Fatal("formatStats() with no activity returned empty string, want a friendly message")
	}
	if strings.Contains(out, "claude") || strings.Contains(out, "opencode") {
		t.Fatalf("formatStats() with no activity = %q, want no per-agent lines at all", out)
	}
}

func TestFormatStats_ReportsDirectAndDelegateCounts(t *testing.T) {
	specs := []session.Spec{{Name: "claude"}, {Name: "opencode"}}
	s := newAgentStats()
	s.direct["claude"] = 5
	s.delegateSent["claude"] = 2
	s.delegateFail["claude"] = 1
	s.delegateRecv["opencode"] = 2

	out := formatStats(specs, s)
	if !strings.Contains(out, "claude") || !strings.Contains(out, "opencode") {
		t.Fatalf("formatStats() = %q, want both agents with activity listed", out)
	}
	if !strings.Contains(out, "5") {
		t.Fatalf("formatStats() = %q, want claude's direct count (5)", out)
	}
	if !strings.Contains(out, "2 (1 failed)") {
		t.Fatalf("formatStats() = %q, want claude's delegate-sent count annotated with its failure count", out)
	}
}

func TestFormatStats_ReportsTokenUsage(t *testing.T) {
	specs := []session.Spec{{Name: "claude"}}
	s := newAgentStats()
	s.usageUsed["claude"] = 45231
	s.usageSize["claude"] = 200000

	out := formatStats(specs, s)
	if !strings.Contains(out, "45231/200000") {
		t.Fatalf("formatStats() = %q, want the raw used/size figures", out)
	}
	if !strings.Contains(out, "22%") {
		t.Fatalf("formatStats() = %q, want the computed percentage (22%%)", out)
	}
}

func TestFormatStats_AgentWithOnlyTokenUsageStillShown(t *testing.T) {
	// A conversational agent that never called a tool but has burned real
	// context still has something worth showing — this is the actual
	// behavior change (2026-09-04, user request): previously formatStats
	// skipped any agent with zero direct/sent/recv activity entirely.
	specs := []session.Spec{{Name: "claude"}}
	s := newAgentStats()
	s.usageUsed["claude"] = 1000
	s.usageSize["claude"] = 200000

	out := formatStats(specs, s)
	if !strings.Contains(out, "claude") {
		t.Fatalf("formatStats() = %q, want claude listed despite having no tool-call activity", out)
	}
	if !strings.Contains(out, "1000/200000") {
		t.Fatalf("formatStats() = %q, want the token usage line", out)
	}
}

func TestFormatStats_TokenUsageWithZeroSizeShowsUsedOnlyNoPanic(t *testing.T) {
	specs := []session.Spec{{Name: "claude"}}
	s := newAgentStats()
	s.usageUsed["claude"] = 500
	s.usageSize["claude"] = 0

	out := formatStats(specs, s)
	if !strings.Contains(out, "500") {
		t.Fatalf("formatStats() = %q, want the used figure shown even with size 0", out)
	}
	if strings.Contains(out, "%") {
		t.Fatalf("formatStats() = %q, want no percentage when size is 0 (would be a divide-by-zero)", out)
	}
}

func TestFormatStats_NoUsageDataOmitsTokenLine(t *testing.T) {
	specs := []session.Spec{{Name: "claude"}}
	s := newAgentStats()
	s.direct["claude"] = 3

	out := formatStats(specs, s)
	if strings.Contains(out, "tokens used") {
		t.Fatalf("formatStats() = %q, want no token line when no usage_update has ever arrived for this agent", out)
	}
}

// --- ACP session modes (`modes`/`mode`/`auto` REPL commands) ------------

func strPtr(s string) *string { return &s }

func TestFormatModes_ListsPerAgentWithCurrentMarked(t *testing.T) {
	specs := []session.Spec{{Name: "claude"}, {Name: "opencode"}}
	claude := newWorker()
	claude.sess = &session.AgentSession{
		AvailableModes: []acp.SessionMode{
			{Id: "default", Name: "Default"},
			{Id: "acceptEdits", Name: "Accept Edits", Description: strPtr("auto-accept edits")},
		},
		CurrentModeId: "acceptEdits",
	}
	workers := map[string]*AgentWorker{"claude": claude, "opencode": newWorker()}

	out := formatModes(specs, workers)
	if !strings.Contains(out, "claude:") {
		t.Fatalf("formatModes() = %q, want a claude section", out)
	}
	if !strings.Contains(out, "* Accept Edits (acceptEdits) — auto-accept edits") {
		t.Fatalf("formatModes() = %q, want the current mode marked with *, name/id/description shown", out)
	}
	if !strings.Contains(out, "  Default (default)") {
		t.Fatalf("formatModes() = %q, want the non-current mode listed unmarked", out)
	}
	if strings.Contains(out, "opencode:") {
		t.Fatalf("formatModes() = %q, want opencode omitted — it reported no AvailableModes", out)
	}
}

func TestFormatModes_NoAgentHasModesReturnsFriendlyMessage(t *testing.T) {
	specs := []session.Spec{{Name: "claude"}}
	workers := map[string]*AgentWorker{"claude": newWorker()}

	out := formatModes(specs, workers)
	if !strings.Contains(out, "no agent has reported") {
		t.Fatalf("formatModes() = %q, want a friendly no-modes message, not blank/panic", out)
	}
}

func TestResolveMode_MatchesById(t *testing.T) {
	w := newWorker()
	w.sess = &session.AgentSession{AvailableModes: []acp.SessionMode{{Id: "acceptEdits", Name: "Accept Edits"}}}

	id, ok := resolveMode(w, "acceptEdits")
	if !ok || id != "acceptEdits" {
		t.Fatalf("resolveMode() = (%q, %v), want (\"acceptEdits\", true)", id, ok)
	}
}

func TestResolveMode_MatchesByNameCaseInsensitive(t *testing.T) {
	w := newWorker()
	w.sess = &session.AgentSession{AvailableModes: []acp.SessionMode{{Id: "acceptEdits", Name: "Accept Edits"}}}

	id, ok := resolveMode(w, "accept edits")
	if !ok || id != "acceptEdits" {
		t.Fatalf("resolveMode() = (%q, %v), want a case-insensitive name match to resolve to id \"acceptEdits\"", id, ok)
	}
}

func TestResolveMode_NoMatchReturnsFalse(t *testing.T) {
	w := newWorker()
	w.sess = &session.AgentSession{AvailableModes: []acp.SessionMode{{Id: "acceptEdits", Name: "Accept Edits"}}}

	if _, ok := resolveMode(w, "nonexistent"); ok {
		t.Fatal("resolveMode() = ok=true for a typed value matching neither id nor name, want false")
	}
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// --- image attachments -------------------------------------------

func TestExtractAttachments_NoAttachments(t *testing.T) {
	clean, images := extractAttachments("just plain text, no @ tokens here")
	if len(images) != 0 {
		t.Fatalf("images = %v, want none", images)
	}
	if clean != "just plain text, no @ tokens here" {
		t.Fatalf("clean = %q, want unchanged text", clean)
	}
}

func TestExtractAttachments_ValidImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, []byte("fake-png"), 0o644); err != nil {
		t.Fatal(err)
	}

	clean, images := extractAttachments("look at @" + path + " please")
	if len(images) != 1 {
		t.Fatalf("images = %v, want exactly 1", images)
	}
	if images[0].Image == nil || images[0].Image.MimeType != "image/png" {
		t.Fatalf("images[0] = %+v, want an image/png block", images[0])
	}
	if clean == "" || containsPath(clean, path) {
		t.Fatalf("clean = %q, want the @path token removed", clean)
	}
}

func TestExtractAttachments_MissingFileLeavesTokenInPlace(t *testing.T) {
	clean, images := extractAttachments("look at @does-not-exist.png please")
	if len(images) != 0 {
		t.Fatalf("images = %v, want none for a missing file", images)
	}
	if !containsPath(clean, "does-not-exist.png") {
		t.Fatalf("clean = %q, want the unresolvable token left in place", clean)
	}
}

func TestExtractAttachments_NonImageExtensionIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, images := extractAttachments("see @" + path)
	if len(images) != 0 {
		t.Fatalf("images = %v, want none — .txt isn't an image extension", images)
	}
	if !containsPath(clean, path) {
		t.Fatalf("clean = %q, want the token left untouched", clean)
	}
}

func TestBuildPromptBlocks_TextOnly(t *testing.T) {
	blocks := buildPromptBlocks("hello world")
	if len(blocks) != 1 || blocks[0].Text == nil || blocks[0].Text.Text != "hello world" {
		t.Fatalf("buildPromptBlocks() = %+v, want a single text block", blocks)
	}
}

// --- generic (non-image) file attachments -------------------------------

func TestExtractFileAttachments_SplicesRealFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("hello from notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := extractFileAttachments("see @" + path)
	if !strings.Contains(got, "hello from notes") {
		t.Fatalf("extractFileAttachments(...) = %q, want the file's content spliced in", got)
	}
	if !containsPath(got, path) {
		t.Fatalf("extractFileAttachments(...) = %q, want the original @path mention preserved", got)
	}
}

func TestExtractFileAttachments_ImageExtensionLeftForExtractAttachments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, []byte("fake-png"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := extractFileAttachments("look at @" + path)
	if strings.Contains(got, "fake-png") {
		t.Fatal("extractFileAttachments spliced an image file's raw bytes into text, want it left untouched for extractAttachments")
	}
}

func TestExtractFileAttachments_UnresolvableTokenLeftAlone(t *testing.T) {
	got := extractFileAttachments("email me at joe@example.com please")
	if got != "email me at joe@example.com please" {
		t.Fatalf("extractFileAttachments(...) = %q, want an unresolvable @-token left completely unchanged", got)
	}
}

func TestExtractFileAttachments_OversizedFileLeftAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	if err := os.WriteFile(path, make([]byte, maxAttachedFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	got := extractFileAttachments("see @" + path)
	if !containsPath(got, path) || strings.Contains(got, "```") {
		t.Fatalf("extractFileAttachments(...) = %q, want an oversized file left as a bare mention, not spliced", got)
	}
}

// --- live "/"-command and "@"-file suggestion popup ---------------------

func TestDetectSuggestToken_SlashOnlyAtLineStartWithNoSpaceYet(t *testing.T) {
	kind, start, token := detectSuggestToken("/pla")
	if kind != suggestCommand || start != 0 || token != "/pla" {
		t.Fatalf("detectSuggestToken(\"/pla\") = (%v,%d,%q), want (suggestCommand,0,\"/pla\")", kind, start, token)
	}
	if kind, _, _ := detectSuggestToken("/plan now"); kind != suggestNone {
		t.Fatalf("detectSuggestToken(\"/plan now\") kind = %v, want suggestNone once a space follows the command name", kind)
	}
	if kind, _, _ := detectSuggestToken("please /plan"); kind != suggestNone {
		t.Fatalf("detectSuggestToken(\"please /plan\") kind = %v, want suggestNone — dispatch() only treats a LEADING slash specially", kind)
	}
}

func TestDetectSuggestToken_AtAnywhereInLine(t *testing.T) {
	kind, start, token := detectSuggestToken("claude: look at @scree")
	if kind != suggestFile || token != "@scree" {
		t.Fatalf("detectSuggestToken(...) = (%v,%d,%q), want (suggestFile,_,\"@scree\")", kind, start, token)
	}
	if want := len("claude: look at "); start != want {
		t.Fatalf("tokenStart = %d, want %d", start, want)
	}
}

func TestDetectSuggestToken_NoneForPlainText(t *testing.T) {
	if kind, _, _ := detectSuggestToken("just talking about email@example.com"); kind != suggestNone {
		t.Fatalf("kind = %v, want suggestNone — no bare @ token at all here", kind)
	}
	if kind, _, _ := detectSuggestToken(""); kind != suggestNone {
		t.Fatalf("kind for empty input = %v, want suggestNone", kind)
	}
}

func TestMatchingCommandSuggestions_FiltersByPrefixAndAnnotatesOwners(t *testing.T) {
	commands := map[string][]acp.AvailableCommand{
		"claude":   {{Name: "plan", Description: "make a plan"}, {Name: "compact", Description: "summarize"}},
		"opencode": {{Name: "plan", Description: "make a plan"}},
	}
	items := matchingCommandSuggestions(commands, "pl")
	if len(items) != 1 {
		t.Fatalf("matchingCommandSuggestions(_, \"pl\") = %+v, want exactly 1 match (\"plan\")", items)
	}
	if items[0].insert != "/plan " {
		t.Fatalf("items[0].insert = %q, want \"/plan \" (trailing space)", items[0].insert)
	}
	if !strings.Contains(items[0].desc, "claude") || !strings.Contains(items[0].desc, "opencode") {
		t.Fatalf("items[0].desc = %q, want both owning agents mentioned", items[0].desc)
	}
}

func TestMatchingCommandSuggestions_NoMatch(t *testing.T) {
	commands := map[string][]acp.AvailableCommand{"claude": {{Name: "plan"}}}
	if items := matchingCommandSuggestions(commands, "zzz"); len(items) != 0 {
		t.Fatalf("matchingCommandSuggestions(_, \"zzz\") = %+v, want none", items)
	}
}

func TestMatchingFileSuggestions_ListsCwdEntriesByPrefix(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "screenshot.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "screens"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	items := matchingFileSuggestions(dir, "scre")
	if len(items) != 2 {
		t.Fatalf("matchingFileSuggestions(dir, \"scre\") = %+v, want 2 matches (screenshot.png, screens/)", items)
	}
	var sawDir, sawFile bool
	for _, it := range items {
		if it.insert == "@screens/" {
			sawDir = true
		}
		if it.insert == "@screenshot.png " {
			sawFile = true
		}
	}
	if !sawDir || !sawFile {
		t.Fatalf("items = %+v, want a directory entry (no trailing space, trailing /) and a file entry (trailing space)", items)
	}
}

func TestMatchingFileSuggestions_NestedDirToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "tui.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	items := matchingFileSuggestions(dir, "internal/tu")
	if len(items) != 1 || items[0].insert != "@internal/tui.go " {
		t.Fatalf("matchingFileSuggestions(dir, \"internal/tu\") = %+v, want [\"@internal/tui.go \"]", items)
	}
}

func TestMatchingFileSuggestions_UnreadableDirReturnsNil(t *testing.T) {
	if items := matchingFileSuggestions(t.TempDir(), "no-such-subdir/anything"); items != nil {
		t.Fatalf("matchingFileSuggestions with an unreadable dir = %+v, want nil", items)
	}
}

func TestBuildPromptBlocks_WithAttachment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, []byte("fake-png"), 0o644); err != nil {
		t.Fatal(err)
	}

	blocks := buildPromptBlocks("describe @" + path)
	if len(blocks) != 2 {
		t.Fatalf("buildPromptBlocks() = %+v, want [image, text]", blocks)
	}
	if blocks[0].Image == nil {
		t.Fatalf("blocks[0] = %+v, want an image block first", blocks[0])
	}
	if blocks[1].Text == nil || blocks[1].Text.Text != "describe" {
		t.Fatalf("blocks[1] = %+v, want text block \"describe\"", blocks[1])
	}
}

func containsPath(s, substr string) bool {
	return indexOf(s, substr) != -1
}

// --- unprefixed prompts go straight to defaultAgent (keyword routing was
// removed entirely — chorus-spec.md §0 — replaced by either "off"/this
// direct path, or an LLM-based decision layered on top by the caller) ---

func TestDispatch_UnprefixedPromptGoesToDefaultAgent(t *testing.T) {
	workers := map[string]*AgentWorker{"opencode": newWorker()}
	agent, sentText, isPrompt, msg, ask, decisionReq := dispatch("list all the markdown files", workers, "opencode", policy.Routing{}, nil)
	if ask != nil {
		t.Fatal("ask != nil, want a direct dispatch, not a routeAsk prompt")
	}
	if decisionReq != nil {
		t.Fatal("decisionReq != nil, want direct dispatch when routing.Mode is off")
	}
	if agent != "opencode" {
		t.Fatalf("agent = %q, want the default agent (opencode)", agent)
	}
	if sentText != "list all the markdown files" {
		t.Fatalf("sentText = %q, want the original prompt text", sentText)
	}
	if !isPrompt {
		t.Fatal("isPrompt = false, want true — an unprefixed free-text turn is preamble-eligible")
	}
	if msg != "" {
		t.Fatalf("msg = %q, want no informational message on the ordinary path", msg)
	}
	// dispatch resolves the destination but no longer queues — the Model
	// (dispatchUserTurn) owns queuing so it can apply the handoff preamble.
	select {
	case blocks := <-workers["opencode"].in:
		t.Fatalf("opencode received %+v — dispatch must not queue anything itself", blocks)
	default:
	}
}

func TestDispatch_ErrorsWhenDefaultAgentNotConnected(t *testing.T) {
	workers := map[string]*AgentWorker{"claude": newWorker()} // opencode not connected
	agent, _, _, msg, ask, _ := dispatch("list all the markdown files", workers, "opencode", policy.Routing{}, nil)
	if agent != "" {
		t.Fatalf("agent = %q, want empty — default agent isn't connected", agent)
	}
	if ask != nil {
		t.Fatal("ask != nil, want a plain error, not a routeAsk prompt")
	}
	if !strings.Contains(msg, "no default agent") {
		t.Fatalf("msg = %q, want the no-default-agent error", msg)
	}
}

func TestDispatch_ErrorsWhenDefaultAgentUnconfigured(t *testing.T) {
	workers := map[string]*AgentWorker{"opencode": newWorker()}
	agent, _, _, msg, ask, _ := dispatch("anything", workers, "", policy.Routing{}, nil)
	if agent != "" {
		t.Fatalf("agent = %q, want empty — no default_agent configured at all", agent)
	}
	if ask != nil {
		t.Fatal("ask != nil, want a plain error, not a routeAsk prompt")
	}
	if !strings.Contains(msg, "no default agent") {
		t.Fatalf("msg = %q, want the no-default-agent error", msg)
	}
}

func TestDispatch_ExplicitAgentOverrideIsNeverSilentlyRerouted(t *testing.T) {
	// gemini isn't connected, but opencode (a valid default) is — an
	// explicit "<agent>: text" override must still fail outright, never
	// silently reroute to a different agent than the one asked for.
	workers := map[string]*AgentWorker{"opencode": newWorker()}
	agent, _, _, msg, ask, _ := dispatch("gemini: do something", workers, "opencode", policy.Routing{}, nil)
	if agent != "" {
		t.Fatalf("agent = %q, want empty — an explicit override to an unconnected agent must fail, not silently reroute", agent)
	}
	if ask != nil {
		t.Fatal("ask != nil, want a plain error, not a routeAsk prompt")
	}
	if !strings.Contains(msg, "unknown agent") {
		t.Fatalf("msg = %q, want the unknown-agent error", msg)
	}
	select {
	case blocks := <-workers["opencode"].in:
		t.Fatalf("opencode received %+v — an explicit override for gemini must never silently fall back to a different agent", blocks)
	default:
	}
}

func TestDispatch_LLMRoutingModeReturnsDecisionRequestInstead(t *testing.T) {
	workers := map[string]*AgentWorker{"opencode": newWorker()}
	routing := policy.Routing{Mode: string(policy.RoutingLLM), DecisionAgent: "opencode"}

	agent, _, _, msg, ask, decisionReq := dispatch("do something", workers, "opencode", routing, nil)
	if agent != "" {
		t.Fatalf("agent = %q, want empty — an LLM routing decision resolves asynchronously, not inside dispatch", agent)
	}
	if msg != "" {
		t.Fatalf("msg = %q, want empty", msg)
	}
	if ask != nil {
		t.Fatal("ask != nil, want nil — this isn't a user-facing routeAsk menu")
	}
	if decisionReq == nil || decisionReq.text != "do something" {
		t.Fatalf("decisionReq = %+v, want a request carrying the original prompt text", decisionReq)
	}
	select {
	case blocks := <-workers["opencode"].in:
		t.Fatalf("opencode received %+v — dispatch must not queue anything itself in llm routing mode", blocks)
	default:
	}
}

func TestDispatch_ExplicitPrefixBypassesLLMRoutingMode(t *testing.T) {
	// An explicit "<agent>: text" override must win even when routing.Mode
	// is "llm" — confirmed with the user: explicit always bypasses routing.
	workers := map[string]*AgentWorker{"claude": newWorker()}
	routing := policy.Routing{Mode: string(policy.RoutingLLM), DecisionAgent: "claude"}

	agent, sentText, _, _, _, decisionReq := dispatch("claude: fix this", workers, "claude", routing, nil)
	if decisionReq != nil {
		t.Fatal("decisionReq != nil, want nil — an explicit prefix must bypass the LLM router entirely")
	}
	if agent != "claude" || sentText != "fix this" {
		t.Fatalf("agent/sentText = %q/%q, want claude/\"fix this\"", agent, sentText)
	}
}
