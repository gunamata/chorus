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
	c := New("claude", outputCh, permCh, pol)

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
	c := New("claude", outputCh, permCh, policy.Policy{}) // edit not auto-allowed

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
	c := New("claude", outputCh, permCh, policy.Policy{})

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
	c := New("claude", outputCh, permCh, pol)

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
	c := New("claude", outputCh, permCh, pol)

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
	c := New("claude", outputCh, permCh, pol)

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
	c := New("claude", outputCh, permCh, policy.Policy{})

	_, err := c.ReadTextFile(context.Background(), acp.ReadTextFileRequest{Path: "relative/path.txt"})
	if err == nil {
		t.Fatal("ReadTextFile() error = nil, want a rejection for a non-absolute path")
	}
	expectNoPermissionRequest(t, permCh)
}

func TestCreateTerminal_AskedAndDeniedNeverStarts(t *testing.T) {
	outputCh := make(chan bus.Update, 8)
	permCh := make(chan bus.PermissionRequest, 1)
	c := New("claude", outputCh, permCh, policy.Policy{}) // execute not auto-allowed

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
	c := New("claude", outputCh, permCh, pol)

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
