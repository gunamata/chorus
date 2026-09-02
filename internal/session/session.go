// Package session implements Connection and AgentSession: one ACP
// client-side connection to one agent subprocess, held for the program's
// whole lifetime (§3), which can host more than one ACP session — the
// main interactive one, plus short-lived delegation sub-sessions (§11)
// that reuse the same already-initialized subprocess instead of spawning
// a new one per delegated call.
//
// Per §6's mitigation for the ACP Go SDK being community-maintained, the
// exported surface here is deliberately narrow (Connect/NewSession/Prompt/
// Close) so the JSON-RPC transport underneath —
// github.com/coder/acp-go-sdk today — could be swapped later without
// touching the router, registry, or REPL. Permission handling isn't a
// method here because it's push-driven by the agent, not pulled by the
// caller: it's wired in at Connect time via an acpclient.Client that
// forwards onto the caller's channels (see chorus-spec.md §7).
package session

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	acp "github.com/coder/acp-go-sdk"

	"chorus/internal/acpclient"
	"chorus/internal/bus"
	"chorus/internal/policy"
)

// cwdToken is substituted in Spec.Args (by Connect) with the real host
// working directory before the subprocess is exec'd — lets a sandboxed
// agent's spawn command line (e.g. `docker run -v {{CWD}}:/workspace ...`)
// stay portable across clones/machines instead of hardcoding an absolute
// path in agents.yaml.
const cwdToken = "{{CWD}}"

// envTokenPattern matches {{ENV:NAME}} in Spec.Args — substituted with
// os.Getenv(NAME) by substituteTokens, the same way cwdToken is. Lets a
// spawn command line reference a machine-specific host path (e.g. a
// sandboxed agent's mounted credential directory) without hardcoding a
// particular user's home directory into agents.yaml — chorus execs argv
// directly with no shell involved, so this substitution is the only place
// such a reference can be expanded; the OS never sees {{ENV:...}} and
// won't expand it itself. An unset variable substitutes as empty string,
// same as an unset shell variable would.
var envTokenPattern = regexp.MustCompile(`\{\{ENV:([A-Za-z_][A-Za-z0-9_]*)\}\}`)

// Spec describes how to launch one agent's ACP subprocess (§10's
// agents.yaml registry).
type Spec struct {
	Name    string
	Command string
	Args    []string

	// WorkDir, if set, is the cwd chorus sends to the agent via ACP's own
	// NewSessionRequest.Cwd/LoadSessionRequest.Cwd (see EffectiveCwd) —
	// for an agent chorus itself runs inside a container (a sandboxed
	// Claude/opencode spawned via `docker run -v {{CWD}}:/workspace ...`),
	// this is the in-container mount point (e.g. "/workspace"), since the
	// container has no way to resolve the host's real path. Empty (the
	// default, non-sandboxed case) leaves today's behavior unchanged: the
	// real host cwd is sent as-is.
	WorkDir string

	// CostTier and Notes are metadata carried through from agents.yaml,
	// surfaced to other agents via the delegate tool's description and
	// the seeded first-turn briefing (internal/delegate) — not used by
	// AgentSession/Connection themselves.
	CostTier string
	Notes    string

	// Models is this agent's declared model catalog (agents.yaml's
	// optional per-agent `models:` list) — consumed by internal/router's
	// LLM-based routing decision to pick a model tier, and by
	// internal/tui to validate a decision's Model field before attempting
	// a model-switch command. Empty/nil is fine: the router simply never
	// picks a model for that agent, only the agent itself.
	Models []ModelInfo
}

// EffectiveCwd returns the cwd to send to this agent via ACP: WorkDir if
// set (the sandboxed case — see WorkDir's doc comment), otherwise the
// real host cwd unchanged.
func (s Spec) EffectiveCwd(hostCwd string) string {
	if s.WorkDir != "" {
		return s.WorkDir
	}
	return hostCwd
}

// substituteTokens replaces every occurrence of cwdToken with cwd and
// every {{ENV:NAME}} with os.Getenv(NAME) in each arg, leaving args with
// no token untouched (the ordinary, non-sandboxed case).
func substituteTokens(args []string, cwd string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		a = strings.ReplaceAll(a, cwdToken, cwd)
		a = envTokenPattern.ReplaceAllStringFunc(a, func(token string) string {
			name := envTokenPattern.FindStringSubmatch(token)[1]
			return os.Getenv(name)
		})
		out[i] = a
	}
	return out
}

// ModelInfo describes one selectable model for an agent, in terms an LLM
// routing decision can use to pick between them — not validated against
// any provider's actual model list, since that would require live API
// access chorus deliberately doesn't have (it only talks to agent CLIs
// over ACP, never a model provider directly).
type ModelInfo struct {
	ID           string `yaml:"id"`
	Label        string `yaml:"label"`
	Capabilities string `yaml:"capabilities"`
	WhenToUse    string `yaml:"when_to_use"`
}

// Connection is one subprocess and its initialized ACP connection. It can
// host multiple sessions (NewSession) over its lifetime.
type Connection struct {
	Name string

	// SupportsLoadSession reports whether this agent advertised the
	// 'loadSession' capability during initialize — i.e. whether
	// LoadSession can be called at all. Populated by Connect.
	SupportsLoadSession bool

	// SupportsImagePrompts reports whether this agent advertised
	// promptCapabilities.image during initialize — i.e. whether an
	// acp.ImageBlock in a Prompt request is expected to be understood
	// rather than just tolerated or rejected. Populated by Connect.
	SupportsImagePrompts bool

	cwd    string
	cmd    *exec.Cmd
	conn   *acp.ClientSideConnection
	client *acpclient.Client
}

// Cwd is the cwd every session opened on this connection should use — the
// spec's EffectiveCwd computed once at Connect time (spec.WorkDir for a
// sandboxed agent, i.e. the in-container mount point like "/workspace";
// the real host cwd otherwise). Any code opening a sub-session on this
// connection (the LLM router's decision sub-session, the delegate hub)
// MUST use this rather than the raw host cwd: a sandboxed agent's ACP
// server accepts a nonexistent host cwd at session/new but then fails the
// first session/prompt with a generic "-32603 Internal error" (found
// live), silently breaking that whole feature for any containerized agent.
func (c *Connection) Cwd() string { return c.cwd }

// Connect starts the agent subprocess and performs the ACP initialize
// handshake. It does not create a session yet — call NewSession or
// LoadSession for that.
//
// stderr receives the subprocess's raw stderr output — deliberately never
// os.Stderr / the live terminal directly (concurrency invariant #3: only
// internal/tui's bubbletea Program writes to the terminal). An agent CLI
// writing an unexpected line to its own stderr — observed live: Gemini's
// free-tier IneligibleTierError dumps a full stack trace plus several
// unrelated startup log lines — would otherwise land raw on the same
// screen bubbletea is actively redrawing, uncoordinated with and
// invisible to chorus's own rendering, only turning up later (once
// bubbletea stops repainting, e.g. at shutdown) looking like it came from
// nowhere. Callers should pass a per-agent log file (main.go does); pass
// io.Discard to drop it entirely rather than risk that.
// delegation and costTiers feed the client-owned delegation-nudge logic
// (CLAUDE.md's delegation-maximization plan item 2,
// acpclient.Client.trackDelegationPreference) — costTiers must include
// every registered agent (not just this one), since the nudge needs to
// know about OTHER agents' cost tiers too. Both are safe to pass
// zero-valued (nil/empty) when the feature isn't configured in
// policy.yaml; the tracking logic no-ops when Delegation.Prefer is empty.
//
// cwd is the real host working directory — substituted for any {{CWD}}
// token found in spec.Args before exec'ing, alongside any {{ENV:NAME}}
// token (see cwdToken/envTokenPattern's doc comments). Non-sandboxed
// spawns with no such tokens are unaffected.
func Connect(ctx context.Context, spec Spec, cwd string, outputCh chan<- bus.Update, permCh chan<- bus.PermissionRequest, pol policy.Policy, delegation policy.Delegation, costTiers map[string]string, stderr io.Writer) (*Connection, error) {
	cmd := exec.CommandContext(ctx, spec.Command, substituteTokens(spec.Args, cwd)...)
	cmd.Stderr = stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%s: stdin pipe: %w", spec.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s: stdout pipe: %w", spec.Name, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: start %q: %w", spec.Name, spec.Command, err)
	}

	client := acpclient.New(spec.Name, outputCh, permCh, pol, delegation, costTiers)
	conn := acp.NewClientSideConnection(client, stdin, stdout)

	initResp, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		},
	})
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("%s: initialize: %w", spec.Name, describeErr(err))
	}

	return &Connection{
		Name:                 spec.Name,
		SupportsLoadSession:  initResp.AgentCapabilities.LoadSession,
		SupportsImagePrompts: initResp.AgentCapabilities.PromptCapabilities.Image,
		cwd:                  spec.EffectiveCwd(cwd),
		cmd:                  cmd,
		conn:                 conn,
		client:               client,
	}, nil
}

// SetIdleChecker wires up the delegation-nudge logic's way of asking
// "is this other agent currently free to take a delegated sub-task?" —
// see acpclient.Client.SetIdler's doc comment for why this has to be a
// post-construction setter rather than a Connect param: it needs
// tui.AgentWorker instances, which don't exist until main.go's phase 3,
// well after every Connection is already established in phase 1.
func (c *Connection) SetIdleChecker(fn func(agent string) bool) {
	c.client.SetIdler(fn)
}

// SetNudgeFunc wires up delivery of a delegation nudge once one's due —
// same post-construction timing as SetIdleChecker. fn is expected to
// queue text as the named agent's next turn (main.go closes over
// tui.QueuePrompt).
func (c *Connection) SetNudgeFunc(fn func(agent, text string)) {
	c.client.SetNudgeFunc(fn)
}

// NewSession creates a fresh session on this connection. mcpServers may be
// nil (no MCP servers attached to this session) — delegation sub-sessions
// (§11) deliberately pass nil so the delegate tool itself is never
// available to them, structurally enforcing the one-hop depth cap instead
// of needing runtime bookkeeping.
func (c *Connection) NewSession(ctx context.Context, cwd string, mcpServers []acp.McpServer) (*AgentSession, error) {
	newSess, err := c.conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: nonNil(mcpServers)})
	if err != nil {
		return nil, fmt.Errorf("%s: session/new: %w", c.Name, describeErr(err))
	}
	return &AgentSession{Name: c.Name, SessionID: newSess.SessionId, conn: c.conn}, nil
}

// LoadSession resumes a previously created session by ID, replaying its
// history back through the same session/update channel every other
// update flows through — so a resumed conversation appears on screen
// exactly like scrollback, not silently. Only call this if
// SupportsLoadSession is true; calling it otherwise returns whatever
// error the agent gives for an unsupported method.
func (c *Connection) LoadSession(ctx context.Context, cwd string, sessionID acp.SessionId, mcpServers []acp.McpServer) (*AgentSession, error) {
	if _, err := c.conn.LoadSession(ctx, acp.LoadSessionRequest{
		Cwd:        cwd,
		SessionId:  sessionID,
		McpServers: nonNil(mcpServers),
	}); err != nil {
		return nil, fmt.Errorf("%s: session/load: %w", c.Name, describeErr(err))
	}
	return &AgentSession{Name: c.Name, SessionID: sessionID, conn: c.conn}, nil
}

// nonNil turns a nil McpServer slice into an empty one. A nil slice
// marshals to JSON `null`; opencode's schema validation rejects that
// ("expected array, received null") on both session/new and session/load
// even though Claude and Gemini tolerate it — confirmed live. Always send
// a real (possibly empty) array.
func nonNil(mcpServers []acp.McpServer) []acp.McpServer {
	if mcpServers == nil {
		return []acp.McpServer{}
	}
	return mcpServers
}

// Close kills the subprocess, ending every session hosted on it.
func (c *Connection) Close() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

// AgentSession is one ACP session on a Connection.
type AgentSession struct {
	Name      string
	SessionID acp.SessionId

	conn *acp.ClientSideConnection
}

// Prompt sends plain text as a user turn. A thin convenience wrapper
// around PromptContent for the common case (used by internal/delegate,
// which only ever hands off plain-text tasks).
func (s *AgentSession) Prompt(ctx context.Context, text string) error {
	return s.PromptContent(ctx, []acp.ContentBlock{acp.TextBlock(text)})
}

// PromptContent sends an arbitrary content-block sequence (text, images,
// ...) as one user turn and blocks until the agent finishes that turn.
// Streamed output arrives concurrently via session/update, forwarded
// through the outputCh given to Connect — not returned here.
func (s *AgentSession) PromptContent(ctx context.Context, blocks []acp.ContentBlock) error {
	_, err := s.conn.Prompt(ctx, acp.PromptRequest{
		SessionId: s.SessionID,
		Prompt:    blocks,
	})
	if err != nil {
		return describeErr(err)
	}
	return nil
}

// Cancel requests the agent stop its current turn.
func (s *AgentSession) Cancel(ctx context.Context) error {
	return s.conn.Cancel(ctx, acp.CancelNotification{SessionId: s.SessionID})
}

func describeErr(err error) error {
	if re, ok := err.(*acp.RequestError); ok {
		return fmt.Errorf("[%d] %s", re.Code, re.Message)
	}
	return err
}
