// Package delegate implements §11's cross-agent delegation: a `delegate`
// MCP tool, attached to every agent's main session, that lets one agent
// hand a sub-task to another and get back its text result — the
// automatic version of manually copy-pasting between agents.
//
// Architecture note (see chorus-spec.md §0 for the full discussion): ACP's
// mcpServers mechanism means the MCP server is spawned by the AGENT
// subprocess itself (e.g. claude-agent-acp spawns it as its own child),
// not by chorus. That spawned process — chorus re-invoked as
// `chorus __mcp_delegate` — is a separate OS process with no shared
// memory with chorus's main process, so it has to reach back into the
// process that actually holds the live AgentSession connections somehow.
// Hub is that reach-back point: a loopback-only HTTP server, address and
// a random per-run bearer token handed to the spawned delegate-mcp
// process via environment variables. This is a deliberate, narrow,
// documented exception to §2's "no sockets" non-goal — not a reintroduced
// daemon, since it only exists for the lifetime of one chorus run and
// only serves delegate-mcp children of that same run.
//
// Hardening note (chorus-spec.md §0's 2026-08-22 audit): the token is the
// only thing standing between "any other local process/user" and this
// endpoint (the listener is loopback-only, but loopback sockets are
// generally reachable by any local process regardless of which OS user
// owns it). See handleDelegate for the specific mitigations added.
package delegate

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"chorus/internal/session"
)

// Environment variable names Hub's mcpServers config sets for the spawned
// delegate-mcp subprocess, and that RunMCPServer reads back.
const (
	EnvAddr   = "CHORUS_DELEGATE_ADDR"
	EnvToken  = "CHORUS_DELEGATE_TOKEN"
	EnvSource = "CHORUS_DELEGATE_SOURCE"
	// EnvRoster carries the JSON-encoded roster of other connected agents
	// (see roster.go) — used to build the delegate tool's description
	// dynamically instead of a generic one.
	EnvRoster = "CHORUS_DELEGATE_ROSTER"
)

// maxTaskBytes bounds a single delegate call's task text — real tasks are
// short prose, not data payloads; this is generous headroom, not a tight
// fit. maxRequestBytes bounds the whole HTTP body (JSON overhead + task).
// maxConcurrentDelegations caps how many delegate calls can be in flight
// at once, so a buggy or adversarially-prompted agent can't spin up
// unbounded sub-sessions (real cost on metered agents, real local
// resource use) by calling delegate() in a tight loop.
const (
	maxTaskBytes             = 100 * 1024
	maxRequestBytes          = 128 * 1024
	maxConcurrentDelegations = 4
)

// LogEntry is one completed (or failed) delegation call, for chorus to
// print via its own single-writer terminal loop — §11 requires every
// delegation call be logged with source, target, task, and a cost
// signal (here, wall-clock duration; token/cost estimates aren't
// available at this layer).
type LogEntry struct {
	Source   string
	Target   string
	Task     string
	Duration time.Duration
	Err      error
}

// Hub is chorus's callback server for delegate-mcp subprocesses.
type Hub struct {
	token    string
	listener net.Listener
	server   *http.Server
	cwd      string
	coll     *Collectors
	logCh    chan<- LogEntry
	conns    map[string]*session.Connection
	sem      chan struct{} // concurrency limiter, see maxConcurrentDelegations
}

// NewHub starts the loopback listener immediately. conns is empty until
// SetConnections is called — Hub must exist (to hand out Addr/Token)
// before agent sessions are created, but agent sessions must exist before
// they're useful delegation targets, so construction and wiring are two
// steps.
func NewHub(cwd string, coll *Collectors, logCh chan<- LogEntry) (*Hub, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("delegate hub: listen: %w", err)
	}
	tok, err := randomToken()
	if err != nil {
		l.Close()
		return nil, fmt.Errorf("delegate hub: token: %w", err)
	}
	h := &Hub{
		token:    tok,
		listener: l,
		cwd:      cwd,
		coll:     coll,
		logCh:    logCh,
		conns:    make(map[string]*session.Connection),
		sem:      make(chan struct{}, maxConcurrentDelegations),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/delegate", h.handleDelegate)
	// Explicit timeouts: the bare http.Serve(l, mux) this replaced had
	// none, so a slow/stalled connection could hold a handler goroutine
	// open indefinitely. WriteTimeout is generous to match callHub's own
	// 5-minute client timeout for a genuinely long agent turn.
	h.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}
	go h.server.Serve(l) //nolint:errcheck // Close() below ends this cleanly
	return h, nil
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Addr is chorus's loopback address for this run, embedded in
// delegate-mcp's spawn env.
func (h *Hub) Addr() string { return h.listener.Addr().String() }

// Token is the shared secret delegate-mcp must present.
func (h *Hub) Token() string { return h.token }

// SetConnections supplies the live agent connections once established.
func (h *Hub) SetConnections(conns map[string]*session.Connection) {
	h.conns = conns
}

// Close stops the HTTP server and its listener.
func (h *Hub) Close() { h.server.Close() }

type delegateRequest struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Task   string `json:"task"`
}

type delegateResponse struct {
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// validToken reports whether the request's Authorization header matches
// h.token, in constant time — a plain != comparison here would leak
// timing information about how many leading bytes matched, in principle
// letting a local attacker recover the token byte-by-byte faster than
// brute force. The token is 128 bits of real entropy, so this is
// defense-in-depth rather than a response to a demonstrated practical
// attack, but the fix costs nothing.
func (h *Hub) validToken(r *http.Request) bool {
	got := []byte(r.Header.Get("Authorization"))
	want := []byte("Bearer " + h.token)
	// subtle.ConstantTimeCompare requires equal-length inputs to be
	// meaningful; unequal lengths are already a safe, immediate reject
	// (and comparing length isn't itself a useful timing oracle).
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func (h *Hub) handleDelegate(w http.ResponseWriter, r *http.Request) {
	if !h.validToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var req delegateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.Task) > maxTaskBytes {
		http.Error(w, fmt.Sprintf("task too large (max %d bytes)", maxTaskBytes), http.StatusRequestEntityTooLarge)
		return
	}

	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		http.Error(w, "too many concurrent delegate calls in flight, try again shortly", http.StatusTooManyRequests)
		return
	}

	start := time.Now()
	result, err := h.doDelegate(r.Context(), req)
	h.logAsync(LogEntry{Source: req.Source, Target: req.Agent, Task: req.Task, Duration: time.Since(start), Err: err})

	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(delegateResponse{Error: err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(delegateResponse{Result: result})
}

// logAsync sends to logCh from a new goroutine rather than the handler
// goroutine directly, so a full log channel (main's select loop draining
// slower than delegate calls complete) blocks nothing but that goroutine
// — never the HTTP handler, which previously could hang a client
// (delegate-mcp, and transitively the agent waiting on it) on a send to
// an unread channel.
func (h *Hub) logAsync(e LogEntry) {
	go func() { h.logCh <- e }()
}

// doDelegate creates a fresh sub-session on the target agent's existing
// connection — not its main conversation, so it doesn't pollute unrelated
// context — with mcpServers deliberately nil. That's what enforces §11's
// one-hop depth cap: the sub-session's agent has no `delegate` tool
// available to it at all, so recursive delegation isn't a permission
// check to get right, it's structurally impossible.
func (h *Hub) doDelegate(ctx context.Context, req delegateRequest) (string, error) {
	conn, ok := h.conns[req.Agent]
	if !ok {
		return "", fmt.Errorf("unknown agent %q", req.Agent)
	}
	sub, err := conn.NewSession(ctx, h.cwd, nil)
	if err != nil {
		return "", err
	}
	h.coll.Register(sub.SessionID)
	promptErr := sub.Prompt(ctx, req.Task)
	text := h.coll.Collect(sub.SessionID)
	if promptErr != nil {
		return "", promptErr
	}
	return text, nil
}
