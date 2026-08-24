// Package acpclient implements the ACP Client role (the wrapper's side of
// the connection): it receives session/update notifications and
// session/request_permission calls from an agent subprocess and forwards
// them onto shared channels so a single goroutine in main can own the
// terminal (see chorus-spec.md §7). It also answers the filesystem and
// terminal RPCs an agent may issue against the client.
//
// Security note (chorus-spec.md §0's 2026-08-22 audit): ReadTextFile,
// WriteTextFile, and CreateTerminal are client-owned mechanical execution
// primitives, distinct from the agent-initiated session/request_permission
// flow. Early versions of this file executed them unconditionally the
// moment an agent asked, which meant policy.yaml's edit/execute ask-by-
// default settings were silently bypassed for any agent that used these
// RPCs instead of its own internal tools. They now route through the same
// policy check (and, if not auto-allowed, the same interactive prompt) as
// RequestPermission — see checkPermission.
package acpclient

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	acp "github.com/coder/acp-go-sdk"

	"chorus/internal/bus"
	"chorus/internal/policy"
)

// Client implements acp.Client for one agent's connection.
type Client struct {
	Agent    string
	OutputCh chan<- bus.Update
	PermCh   chan<- bus.PermissionRequest
	Policy   policy.Policy

	mu        sync.Mutex
	terminals map[string]*terminal
}

var _ acp.Client = (*Client)(nil)

func New(agent string, outputCh chan<- bus.Update, permCh chan<- bus.PermissionRequest, pol policy.Policy) *Client {
	return &Client{
		Agent:     agent,
		OutputCh:  outputCh,
		PermCh:    permCh,
		Policy:    pol,
		terminals: make(map[string]*terminal),
	}
}

// SessionUpdate forwards the notification to the shared output channel.
// Kept purely mechanical per §7/§15 — rendering happens elsewhere.
func (c *Client) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	c.OutputCh <- bus.Update{Agent: c.Agent, Notification: params}
	return nil
}

// RequestPermission checks the policy allow-list first; if the tool call's
// kind isn't auto-allowed, it routes the request through permCh and blocks
// until the single terminal-owning goroutine answers it. This call runs on
// the agent connection's own read loop, so blocking here blocks only this
// agent's turn — the other agent keeps running (§7).
func (c *Client) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	kind := ""
	if params.ToolCall.Kind != nil {
		kind = string(*params.ToolCall.Kind)
	}
	title := ""
	if params.ToolCall.Title != nil {
		title = *params.ToolCall.Title
	}
	if c.Policy.AutoAllow(c.Agent, kind) || c.Policy.AutoAllowTool(c.Agent, title) {
		if resp, ok := firstAllowOption(params.Options); ok {
			return resp, nil
		}
		// Policy said allow but the agent offered no allow-shaped option;
		// fall through and ask rather than guess.
	}

	resp := make(chan acp.RequestPermissionResponse, 1)
	c.PermCh <- bus.PermissionRequest{Agent: c.Agent, Req: params, Resp: resp}
	select {
	case r := <-resp:
		return r, nil
	case <-ctx.Done():
		return acp.RequestPermissionResponse{
			Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}},
		}, ctx.Err()
	}
}

func firstAllowOption(options []acp.PermissionOption) (acp.RequestPermissionResponse, bool) {
	for _, o := range options {
		if o.Kind == acp.PermissionOptionKindAllowOnce || o.Kind == acp.PermissionOptionKindAllowAlways {
			return acp.RequestPermissionResponse{
				Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: o.OptionId}},
			}, true
		}
	}
	return acp.RequestPermissionResponse{}, false
}

// checkPermission is the client-owned counterpart to RequestPermission:
// used for RPCs the agent invokes directly (ReadTextFile, WriteTextFile,
// CreateTerminal) that carry no permission request of their own. It checks
// the policy allow-list for kind first; if not auto-allowed, it synthesizes
// a two-option (allow/deny) permission request and routes it through the
// exact same permCh -> main-loop-prompt -> resp flow RequestPermission
// uses, so it reuses the existing UI without any changes there.
func (c *Client) checkPermission(ctx context.Context, kind acp.ToolKind, title string, sessionID acp.SessionId) (bool, error) {
	if c.Policy.AutoAllow(c.Agent, string(kind)) {
		return true, nil
	}

	k := kind
	req := acp.RequestPermissionRequest{
		SessionId: sessionID,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(fmt.Sprintf("%s-client-op-%d", c.Agent, time.Now().UnixNano())),
			Title:      &title,
			Kind:       &k,
		},
		Options: []acp.PermissionOption{
			{OptionId: "allow", Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: "deny", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
		},
	}

	resp := make(chan acp.RequestPermissionResponse, 1)
	c.PermCh <- bus.PermissionRequest{Agent: c.Agent, Req: req, Resp: resp}
	select {
	case r := <-resp:
		return r.Outcome.Selected != nil && r.Outcome.Selected.OptionId == "allow", nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (c *Client) ReadTextFile(ctx context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.ReadTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}
	if isUNCPath(params.Path) {
		return acp.ReadTextFileResponse{}, fmt.Errorf("refusing to access network path: %s", params.Path)
	}
	allowed, err := c.checkPermission(ctx, acp.ToolKindRead, "Read file: "+params.Path, params.SessionId)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	if !allowed {
		return acp.ReadTextFileResponse{}, fmt.Errorf("permission denied by user")
	}

	b, err := os.ReadFile(params.Path)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	content := string(b)
	if params.Line != nil || params.Limit != nil {
		lines := strings.Split(content, "\n")
		start := 0
		if params.Line != nil && *params.Line > 0 {
			start = *params.Line - 1
		}
		start = clamp(start, 0, len(lines))
		end := len(lines)
		if params.Limit != nil && *params.Limit > 0 && start+*params.Limit < end {
			end = start + *params.Limit
		}
		content = strings.Join(lines[start:end], "\n")
	}
	return acp.ReadTextFileResponse{Content: content}, nil
}

func (c *Client) WriteTextFile(ctx context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.WriteTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}
	if isUNCPath(params.Path) {
		return acp.WriteTextFileResponse{}, fmt.Errorf("refusing to access network path: %s", params.Path)
	}
	allowed, err := c.checkPermission(ctx, acp.ToolKindEdit, "Write file: "+params.Path, params.SessionId)
	if err != nil {
		return acp.WriteTextFileResponse{}, err
	}
	if !allowed {
		return acp.WriteTextFileResponse{}, fmt.Errorf("permission denied by user")
	}

	if dir := filepath.Dir(params.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return acp.WriteTextFileResponse{}, err
		}
	}
	if err := os.WriteFile(params.Path, []byte(params.Content), 0o644); err != nil {
		return acp.WriteTextFileResponse{}, err
	}
	return acp.WriteTextFileResponse{}, nil
}

// isUNCPath reports whether path is a Windows UNC network path
// (\\host\share\... or //host/share/...). filepath.IsAbs treats these as
// absolute, but honoring a read/write request for one causes Windows to
// silently attempt an SMB connection — authenticating automatically with
// the current user's credentials (NTLM), a well-known technique for
// capturing or relaying credentials without any code execution at all.
// There's no legitimate reason a coding agent's file access needs network
// shares, so these are rejected outright rather than left to the
// permission prompt.
func isUNCPath(path string) bool {
	return strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, `//`)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// --- Terminal capability -----------------------------------------------
//
// Some agents (Gemini CLI's --acp mode in particular) execute shell
// commands by asking the *client* to run them via terminal/create rather
// than shelling out themselves, so this needs to be functional, not a
// stub, for those agents to actually run commands.

const defaultTerminalOutputLimit = 2 * 1024 * 1024 // 2MB, used when the agent doesn't specify outputByteLimit

type terminal struct {
	cmd       *exec.Cmd
	mu        sync.Mutex
	out       []byte
	limit     int
	truncated bool
	done      chan struct{}
	exit      acp.TerminalExitStatus
}

type terminalWriter struct{ t *terminal }

// Write appends to the terminal's captured output, truncating from the
// beginning once it exceeds t.limit — per ACP's CreateTerminalRequest.
// OutputByteLimit doc: "the Client truncates from the beginning... MUST
// ensure truncation happens at a character boundary." Also bounds memory
// use for long-running or very chatty commands, which grew unbounded
// before this existed.
func (w *terminalWriter) Write(p []byte) (int, error) {
	w.t.mu.Lock()
	w.t.out = append(w.t.out, p...)
	if w.t.limit > 0 && len(w.t.out) > w.t.limit {
		excess := len(w.t.out) - w.t.limit
		for excess < len(w.t.out) && !utf8.RuneStart(w.t.out[excess]) {
			excess++
		}
		w.t.out = w.t.out[excess:]
		w.t.truncated = true
	}
	w.t.mu.Unlock()
	return len(p), nil
}

func (c *Client) CreateTerminal(ctx context.Context, params acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	title := strings.TrimSpace(fmt.Sprintf("Run: %s %s", params.Command, strings.Join(params.Args, " ")))
	allowed, err := c.checkPermission(ctx, acp.ToolKindExecute, title, params.SessionId)
	if err != nil {
		return acp.CreateTerminalResponse{}, err
	}
	if !allowed {
		return acp.CreateTerminalResponse{}, fmt.Errorf("permission denied by user")
	}

	cmd := exec.Command(params.Command, params.Args...)
	if params.Cwd != nil {
		cmd.Dir = *params.Cwd
	}
	env := os.Environ()
	for _, e := range params.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	cmd.Env = env

	limit := defaultTerminalOutputLimit
	if params.OutputByteLimit != nil && *params.OutputByteLimit > 0 {
		limit = *params.OutputByteLimit
	}
	t := &terminal{cmd: cmd, done: make(chan struct{}), limit: limit}
	w := &terminalWriter{t: t}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		return acp.CreateTerminalResponse{}, err
	}

	id := fmt.Sprintf("%s-term-%d", c.Agent, time.Now().UnixNano())
	c.mu.Lock()
	c.terminals[id] = t
	c.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		t.mu.Lock()
		if cmd.ProcessState != nil {
			code := cmd.ProcessState.ExitCode()
			t.exit.ExitCode = &code
		}
		t.mu.Unlock()
		close(t.done)
	}()

	return acp.CreateTerminalResponse{TerminalId: id}, nil
}

func (c *Client) getTerminal(id string) (*terminal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.terminals[id]
	return t, ok
}

func (c *Client) TerminalOutput(ctx context.Context, params acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	t, ok := c.getTerminal(params.TerminalId)
	if !ok {
		return acp.TerminalOutputResponse{}, fmt.Errorf("unknown terminal: %s", params.TerminalId)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	resp := acp.TerminalOutputResponse{Output: string(t.out), Truncated: t.truncated}
	select {
	case <-t.done:
		exit := t.exit
		resp.ExitStatus = &exit
	default:
	}
	return resp, nil
}

func (c *Client) WaitForTerminalExit(ctx context.Context, params acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	t, ok := c.getTerminal(params.TerminalId)
	if !ok {
		return acp.WaitForTerminalExitResponse{}, fmt.Errorf("unknown terminal: %s", params.TerminalId)
	}
	select {
	case <-t.done:
	case <-ctx.Done():
		return acp.WaitForTerminalExitResponse{}, ctx.Err()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return acp.WaitForTerminalExitResponse{ExitCode: t.exit.ExitCode, Signal: t.exit.Signal}, nil
}

func (c *Client) ReleaseTerminal(ctx context.Context, params acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	c.mu.Lock()
	delete(c.terminals, params.TerminalId)
	c.mu.Unlock()
	return acp.ReleaseTerminalResponse{}, nil
}

func (c *Client) KillTerminal(ctx context.Context, params acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	t, ok := c.getTerminal(params.TerminalId)
	if !ok {
		return acp.KillTerminalResponse{}, nil
	}
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	return acp.KillTerminalResponse{}, nil
}
