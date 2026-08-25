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
	"sort"
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
	Agent      string
	OutputCh   chan<- bus.Update
	PermCh     chan<- bus.PermissionRequest
	Policy     policy.Policy
	Delegation policy.Delegation
	// CostTiers maps every registered agent's name (including Agent
	// itself) to its agents.yaml cost_tier — needed to tell whether Agent
	// is the metered one and whether any OTHER connected agent is cheap
	// enough to nudge toward. Populated once at construction (the full
	// registry is known before any agent connects — see main.go), unlike
	// idler/nudgeFn below, which can only be wired up once tui.AgentWorker
	// instances exist (main.go's phase 3, after every Connection).
	CostTiers map[string]string

	mu          sync.Mutex
	terminals   map[string]*terminal
	nudgeCounts map[string]int
	idler       func(agent string) bool
	nudgeFn     func(agent, text string)
}

var _ acp.Client = (*Client)(nil)

func New(agent string, outputCh chan<- bus.Update, permCh chan<- bus.PermissionRequest, pol policy.Policy, delegation policy.Delegation, costTiers map[string]string) *Client {
	return &Client{
		Agent:       agent,
		OutputCh:    outputCh,
		PermCh:      permCh,
		Policy:      pol,
		Delegation:  delegation,
		CostTiers:   costTiers,
		terminals:   make(map[string]*terminal),
		nudgeCounts: make(map[string]int),
	}
}

// SetIdler wires up the callback trackDelegationPreference uses to ask
// whether another agent is currently free to take a delegated sub-task.
// Called post-construction (session.Connection.SetIdleChecker) once
// tui.AgentWorker instances exist — Client itself is constructed earlier,
// in Connect, before any worker does.
func (c *Client) SetIdler(fn func(agent string) bool) {
	c.mu.Lock()
	c.idler = fn
	c.mu.Unlock()
}

// SetNudgeFunc wires up the callback trackDelegationPreference invokes
// once a delegation nudge is actually due. Same post-construction timing
// as SetIdler.
func (c *Client) SetNudgeFunc(fn func(agent, text string)) {
	c.mu.Lock()
	c.nudgeFn = fn
	c.mu.Unlock()
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
	c.trackDelegationPreference(kind, title)
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

// trackDelegationPreference is CLAUDE.md's delegation-maximization plan
// item 2: since ACP's RequestPermissionResponse carries only an
// Outcome (Selected{OptionId} or Cancelled) — no free-text field — there
// is no protocol-level way to "redirect" a tool call toward delegating
// instead. The only channel that can ever put a suggestion in front of an
// agent is a normal prompt turn, so this counts direct Prefer-kind tool
// calls since the acting agent's last `delegate` call and, once
// Delegation.Threshold() is reached, fires nudgeFn with suggestion text
// for the caller to queue as that agent's next turn (see
// session.Connection.SetNudgeFunc / main.go's wiring). This runs on every
// RequestPermission/checkPermission call regardless of allow/deny — an
// agent that keeps asking-and-being-approved for mechanical work directly
// is exactly the pattern worth nudging, not just auto-allowed calls.
func (c *Client) trackDelegationPreference(kind, title string) {
	if len(c.Delegation.Prefer) == 0 {
		return
	}
	if c.Policy.AutoAllowTool(c.Agent, title) {
		// This IS a delegate call (auto_allow_tools is, by this project's
		// own convention, scoped to exactly that tool) — the agent just
		// did the preferred thing, so the streak resets.
		c.mu.Lock()
		delete(c.nudgeCounts, c.Agent)
		c.mu.Unlock()
		return
	}
	if c.CostTiers[c.Agent] != "metered" || !c.Delegation.PreferKind(kind) {
		return
	}

	// Count every direct Prefer-kind call since the last delegate call
	// first, regardless of whether a cheaper agent happens to be idle
	// right now — idleCheaperAgents is only consulted once the streak is
	// ready to fire. Checking it before incrementing (as this used to)
	// meant a run of direct calls made while every non-metered agent was
	// busy didn't count at all, undercounting the streak this function's
	// own doc comment claims to track.
	c.mu.Lock()
	c.nudgeCounts[c.Agent]++
	ready := c.nudgeCounts[c.Agent] >= c.Delegation.Threshold()
	c.mu.Unlock()
	if !ready {
		return
	}

	idleCheaper := c.idleCheaperAgents()
	if len(idleCheaper) == 0 {
		// Threshold reached but nothing to suggest yet — leave the counter
		// at/above threshold so this fires the moment a cheaper agent goes
		// idle, instead of resetting and losing the streak.
		return
	}

	c.mu.Lock()
	delete(c.nudgeCounts, c.Agent)
	fn := c.nudgeFn
	c.mu.Unlock()

	if fn != nil {
		fn(c.Agent, buildNudgeText(kind, idleCheaper))
	}
}

// idleCheaperAgents returns every OTHER registered agent whose cost_tier
// isn't "metered" and whose worker is currently idle, in CostTiers'
// (map, so unordered) iteration order sorted for determinism. Returns nil
// if idler was never wired up (SetIdler not yet called — a narrow startup
// window, see main.go) rather than treating "unknown" as "idle."
func (c *Client) idleCheaperAgents() []string {
	c.mu.Lock()
	idler := c.idler
	c.mu.Unlock()
	if idler == nil {
		return nil
	}
	var out []string
	for name, tier := range c.CostTiers {
		if name == c.Agent || tier == "metered" {
			continue
		}
		if idler(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// buildNudgeText builds the suggestion queued as the metered agent's next
// turn once trackDelegationPreference's threshold is hit. Opens with the
// same explicit self-identification buildBriefingText (main.go) uses —
// found live (chorus-spec.md §0) to be necessary for at least one agent
// (opencode) to not spend a turn suspecting a plain instruction message
// is a prompt-injection attempt.
func buildNudgeText(kind string, idleCheaper []string) string {
	return fmt.Sprintf(
		"This is an automated note from chorus itself, not from the user — no response needed, nothing to "+
			"acknowledge. You've made several %q-kind tool calls directly in a row. %s currently idle and "+
			"reachable through your `delegate` tool — consider delegating similar mechanical sub-tasks to "+
			"one of them going forward, when the work doesn't depend on context only you have.",
		kind, idleAgentsClause(idleCheaper))
}

// idleAgentsClause renders idleCheaper as a short clause, singular or
// plural, e.g. "opencode is" or "gemini and opencode are".
func idleAgentsClause(idleCheaper []string) string {
	switch len(idleCheaper) {
	case 0:
		return "another connected agent is"
	case 1:
		return idleCheaper[0] + " is"
	default:
		return strings.Join(idleCheaper[:len(idleCheaper)-1], ", ") + " and " + idleCheaper[len(idleCheaper)-1] + " are"
	}
}

// checkPermission is the client-owned counterpart to RequestPermission:
// used for RPCs the agent invokes directly (ReadTextFile, WriteTextFile,
// CreateTerminal) that carry no permission request of their own. It checks
// the policy allow-list for kind first; if not auto-allowed, it synthesizes
// a two-option (allow/deny) permission request and routes it through the
// exact same permCh -> main-loop-prompt -> resp flow RequestPermission
// uses, so it reuses the existing UI without any changes there.
func (c *Client) checkPermission(ctx context.Context, kind acp.ToolKind, title string, sessionID acp.SessionId) (bool, error) {
	c.trackDelegationPreference(string(kind), title)
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
