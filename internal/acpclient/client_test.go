package acpclient

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"chorus/internal/bus"
	"chorus/internal/policy"
)

// echoCommand returns a command/args pair that prints text to stdout,
// portable between Windows (this project's primary target) and Unix.
func echoCommand(text string) (cmd string, args []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/C", "echo " + text}
	}
	return "sh", []string{"-c", "echo " + text}
}

// answerNextPermission drains one bus.PermissionRequest from permCh and
// answers it with optionID, simulating the user. Fails the test if no
// request arrives within the timeout — used to assert that a gated
// operation actually asked, rather than silently proceeding.
func answerNextPermission(t *testing.T, permCh chan bus.PermissionRequest, optionID acp.PermissionOptionId) {
	t.Helper()
	select {
	case req := <-permCh:
		req.Resp <- acp.RequestPermissionResponse{
			Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: optionID}},
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a permission request, got none")
	}
}

// expectNoPermissionRequest asserts nothing arrives on permCh — used to
// verify auto-allowed operations never interrupt the user at all.
func expectNoPermissionRequest(t *testing.T, permCh chan bus.PermissionRequest) {
	t.Helper()
	select {
	case req := <-permCh:
		t.Fatalf("unexpected permission request: %+v", req.Req)
	case <-time.After(100 * time.Millisecond):
	}
}

// Regression tests for chorus-spec.md §0's 2026-08-22 audit finding:
// ReadTextFile/WriteTextFile/CreateTerminal used to execute unconditionally
// the moment an agent asked, completely bypassing policy.yaml. They now
// route through checkPermission — the same policy-check-then-prompt flow
// RequestPermission already used.

func TestWriteTextFile_AutoAllowedSkipsPrompt(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 1)
	pol := policy.Policy{"claude": policy.AgentPolicy{AutoAllow: []string{"edit"}}}
	c := New("claude", outputCh, permCh, pol, policy.Delegation{}, nil)

	path := filepath.Join(t.TempDir(), "out.txt")
	_, err := c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{Path: path, Content: "hello"})
	if err != nil {
		t.Fatalf("WriteTextFile() error = %v, want nil (edit is auto-allowed)", err)
	}
	expectNoPermissionRequest(t, permCh)

	got, err := os.ReadFile(path)
	if err != nil || string(got) != "hello" {
		t.Fatalf("file content = (%q, %v), want (hello, nil)", got, err)
	}
}

func TestWriteTextFile_AskedAndDeniedDoesNotWrite(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 1)
	c := New("claude", outputCh, permCh, policy.Policy{}, policy.Delegation{}, nil) // edit not auto-allowed

	path := filepath.Join(t.TempDir(), "out.txt")
	done := make(chan error, 1)
	go func() {
		_, err := c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{Path: path, Content: "hello"})
		done <- err
	}()

	answerNextPermission(t, permCh, "deny")

	err := <-done
	if err == nil {
		t.Fatal("WriteTextFile() error = nil, want a permission-denied error")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("file was written despite denied permission (stat err = %v)", statErr)
	}
}

func TestWriteTextFile_AskedAndAllowedWrites(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 1)
	c := New("claude", outputCh, permCh, policy.Policy{}, policy.Delegation{}, nil)

	path := filepath.Join(t.TempDir(), "out.txt")
	done := make(chan error, 1)
	go func() {
		_, err := c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{Path: path, Content: "hello"})
		done <- err
	}()

	answerNextPermission(t, permCh, "allow")

	if err := <-done; err != nil {
		t.Fatalf("WriteTextFile() error = %v, want nil", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "hello" {
		t.Fatalf("file content = (%q, %v), want (hello, nil)", got, err)
	}
}

func TestWriteTextFile_RejectsUNCPath_NeverAsks(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 1)
	pol := policy.Policy{"claude": policy.AgentPolicy{AutoAllow: []string{"edit"}}} // even if auto-allowed
	c := New("claude", outputCh, permCh, pol, policy.Delegation{}, nil)

	_, err := c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
		Path:    `\\attacker.example.com\share\payload.txt`,
		Content: "hello",
	})
	if err == nil {
		t.Fatal("WriteTextFile() error = nil, want a rejection for a UNC path")
	}
	expectNoPermissionRequest(t, permCh)
}

func TestReadTextFile_RejectsUNCPath(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 1)
	pol := policy.Policy{"claude": policy.AgentPolicy{AutoAllow: []string{"read"}}}
	c := New("claude", outputCh, permCh, pol, policy.Delegation{}, nil)

	_, err := c.ReadTextFile(context.Background(), acp.ReadTextFileRequest{
		Path: `\\attacker.example.com\share\secret.txt`,
	})
	if err == nil {
		t.Fatal("ReadTextFile() error = nil, want a rejection for a UNC path")
	}
	expectNoPermissionRequest(t, permCh)
}

func TestReadTextFile_RejectsForwardSlashUNCPath(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 1)
	pol := policy.Policy{"claude": policy.AgentPolicy{AutoAllow: []string{"read"}}}
	c := New("claude", outputCh, permCh, pol, policy.Delegation{}, nil)

	_, err := c.ReadTextFile(context.Background(), acp.ReadTextFileRequest{
		Path: `//attacker.example.com/share/secret.txt`,
	})
	if err == nil {
		t.Fatal("ReadTextFile() error = nil, want a rejection for a //-style UNC path")
	}
	expectNoPermissionRequest(t, permCh)
}

func TestReadTextFile_RejectsRelativePath(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 1)
	c := New("claude", outputCh, permCh, policy.Policy{}, policy.Delegation{}, nil)

	_, err := c.ReadTextFile(context.Background(), acp.ReadTextFileRequest{Path: "relative/path.txt"})
	if err == nil {
		t.Fatal("ReadTextFile() error = nil, want a rejection for a non-absolute path")
	}
	expectNoPermissionRequest(t, permCh)
}

func TestCreateTerminal_AskedAndDeniedNeverStarts(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 1)
	c := New("claude", outputCh, permCh, policy.Policy{}, policy.Delegation{}, nil) // execute not auto-allowed

	done := make(chan error, 1)
	go func() {
		_, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{Command: "does-not-matter"})
		done <- err
	}()

	answerNextPermission(t, permCh, "deny")

	if err := <-done; err == nil {
		t.Fatal("CreateTerminal() error = nil, want a permission-denied error")
	}
}

func TestCreateTerminal_AutoAllowedRunsImmediately(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 1)
	pol := policy.Policy{"claude": policy.AgentPolicy{AutoAllow: []string{"execute"}}}
	c := New("claude", outputCh, permCh, pol, policy.Delegation{}, nil)

	cmd, args := echoCommand("audit-ok")
	resp, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{Command: cmd, Args: args})
	if err != nil {
		t.Fatalf("CreateTerminal() error = %v, want nil (execute is auto-allowed)", err)
	}
	expectNoPermissionRequest(t, permCh)
	if resp.TerminalId == "" {
		t.Fatal("TerminalId is empty, want a real terminal handle")
	}

	waitForTerminalDone(t, c, resp.TerminalId)
	out, err := c.TerminalOutput(context.Background(), acp.TerminalOutputRequest{TerminalId: resp.TerminalId})
	if err != nil {
		t.Fatalf("TerminalOutput() error = %v", err)
	}
	if !contains(out.Output, "audit-ok") {
		t.Fatalf("terminal output = %q, want it to contain %q", out.Output, "audit-ok")
	}
}

// --- delegation-nudge tests (CLAUDE.md's delegation-maximization plan
// item 2) — trackDelegationPreference, exercised through RequestPermission
// since that's the code path every agent-initiated tool call goes
// through, and it's the simplest way to drive kind/title without needing
// a real subprocess.

// requestPermission builds a minimal allow-by-default RequestPermission
// call for kind/title, options offering both allow and deny, and returns
// once answered (or immediately if auto-allowed) — a test helper for the
// nudge tests below, which care about trackDelegationPreference's side
// effects, not the specific outcome of any one call.
func requestPermission(t *testing.T, c *Client, permCh chan bus.PermissionRequest, kind acp.ToolKind, title string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := c.RequestPermission(context.Background(), acp.RequestPermissionRequest{
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "tc",
				Title:      &title,
				Kind:       &kind,
			},
			Options: []acp.PermissionOption{
				{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionId: "deny", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
			},
		})
		done <- err
	}()
	select {
	case req := <-permCh:
		req.Resp <- acp.RequestPermissionResponse{
			Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: "allow"}},
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a permission request, got none")
	}
	if err := <-done; err != nil {
		t.Fatalf("RequestPermission() error = %v", err)
	}
}

func TestDelegationNudge_FiresAtThreshold(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 4)
	delegation := policy.Delegation{Prefer: []string{"execute"}, NudgeThreshold: intPtr(3)}
	costTiers := map[string]string{"claude": "metered", "opencode": "free"}
	c := New("claude", outputCh, permCh, policy.Policy{}, delegation, costTiers)
	c.SetIdler(func(agent string) bool { return agent == "opencode" })

	var nudges []string
	c.SetNudgeFunc(func(agent, text string) { nudges = append(nudges, agent+": "+text) })

	for i := 0; i < 2; i++ {
		requestPermission(t, c, permCh, acp.ToolKindExecute, "run something")
		if len(nudges) != 0 {
			t.Fatalf("nudge fired after only %d calls, want it to wait for the configured threshold (3)", i+1)
		}
	}
	requestPermission(t, c, permCh, acp.ToolKindExecute, "run something")
	if len(nudges) != 1 {
		t.Fatalf("nudges = %v, want exactly 1 after hitting the threshold", nudges)
	}
	if !contains(nudges[0], "opencode") {
		t.Fatalf("nudge text = %q, want it to name the idle cheaper agent (opencode)", nudges[0])
	}

	// Counter resets after firing — the next 2 calls shouldn't fire again.
	for i := 0; i < 2; i++ {
		requestPermission(t, c, permCh, acp.ToolKindExecute, "run something")
	}
	if len(nudges) != 1 {
		t.Fatalf("nudges = %v, want still exactly 1 (counter should reset after firing)", nudges)
	}
}

func TestDelegationNudge_DelegateCallResetsCounter(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 4)
	delegation := policy.Delegation{Prefer: []string{"execute"}, NudgeThreshold: intPtr(3)}
	costTiers := map[string]string{"claude": "metered", "opencode": "free"}
	pol := policy.Policy{"claude": policy.AgentPolicy{AutoAllowTools: []string{"mcp__chorus-delegate__"}}}
	c := New("claude", outputCh, permCh, pol, delegation, costTiers)
	c.SetIdler(func(agent string) bool { return agent == "opencode" })

	var nudges []string
	c.SetNudgeFunc(func(agent, text string) { nudges = append(nudges, text) })

	requestPermission(t, c, permCh, acp.ToolKindExecute, "run something")
	requestPermission(t, c, permCh, acp.ToolKindExecute, "run something")
	// A delegate call (auto-allowed by title prefix, per policy.yaml
	// convention, so it never reaches permCh at all) — should reset the
	// streak instead of counting toward it.
	kind := acp.ToolKindOther
	title := "mcp__chorus-delegate__delegate"
	if _, err := c.RequestPermission(context.Background(), acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{ToolCallId: "tc", Title: &title, Kind: &kind},
		Options: []acp.PermissionOption{
			{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
		},
	}); err != nil {
		t.Fatalf("RequestPermission() error = %v", err)
	}
	requestPermission(t, c, permCh, acp.ToolKindExecute, "run something")
	requestPermission(t, c, permCh, acp.ToolKindExecute, "run something")

	if len(nudges) != 0 {
		t.Fatalf("nudges = %v, want none — the delegate call should have reset the streak below the threshold", nudges)
	}
}

func TestDelegationNudge_NoIdleCheaperAgent_NeverFires(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 4)
	delegation := policy.Delegation{Prefer: []string{"execute"}, NudgeThreshold: intPtr(1)}
	costTiers := map[string]string{"claude": "metered", "opencode": "free"}
	c := New("claude", outputCh, permCh, policy.Policy{}, delegation, costTiers)
	c.SetIdler(func(agent string) bool { return false }) // opencode busy

	var nudges []string
	c.SetNudgeFunc(func(agent, text string) { nudges = append(nudges, text) })

	requestPermission(t, c, permCh, acp.ToolKindExecute, "run something")
	if len(nudges) != 0 {
		t.Fatalf("nudges = %v, want none — no cheaper agent is idle", nudges)
	}
}

func TestDelegationNudge_NonMeteredAgent_NeverFires(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 4)
	delegation := policy.Delegation{Prefer: []string{"execute"}, NudgeThreshold: intPtr(1)}
	costTiers := map[string]string{"opencode": "free", "claude": "metered"}
	c := New("opencode", outputCh, permCh, policy.Policy{}, delegation, costTiers)
	c.SetIdler(func(agent string) bool { return true })

	var nudges []string
	c.SetNudgeFunc(func(agent, text string) { nudges = append(nudges, text) })

	requestPermission(t, c, permCh, acp.ToolKindExecute, "run something")
	if len(nudges) != 0 {
		t.Fatalf("nudges = %v, want none — opencode itself isn't the metered agent", nudges)
	}
}

func TestDelegationNudge_EmptyPreferDisablesTrackingEntirely(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 4)
	costTiers := map[string]string{"claude": "metered", "opencode": "free"}
	c := New("claude", outputCh, permCh, policy.Policy{}, policy.Delegation{}, costTiers) // Prefer unset
	c.SetIdler(func(agent string) bool { return true })

	nudgeCalled := false
	c.SetNudgeFunc(func(agent, text string) { nudgeCalled = true })

	for i := 0; i < 10; i++ {
		requestPermission(t, c, permCh, acp.ToolKindExecute, "run something")
	}
	if nudgeCalled {
		t.Fatal("nudge fired despite an empty Delegation.Prefer, which should disable tracking entirely")
	}
}

func TestDelegationNudge_UnmatchedKind_NeverCounts(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 4)
	delegation := policy.Delegation{Prefer: []string{"execute"}, NudgeThreshold: intPtr(1)}
	costTiers := map[string]string{"claude": "metered", "opencode": "free"}
	c := New("claude", outputCh, permCh, policy.Policy{}, delegation, costTiers)
	c.SetIdler(func(agent string) bool { return true })

	nudgeCalled := false
	c.SetNudgeFunc(func(agent, text string) { nudgeCalled = true })

	requestPermission(t, c, permCh, acp.ToolKindRead, "read a file") // "read" isn't in Prefer
	if nudgeCalled {
		t.Fatal("nudge fired for a tool kind not listed in Delegation.Prefer")
	}
}

func intPtr(n int) *int { return &n }

func waitForTerminalDone(t *testing.T, c *Client, id string) {
	t.Helper()
	_, err := c.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{TerminalId: id})
	if err != nil {
		t.Fatalf("WaitForTerminalExit() error = %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
