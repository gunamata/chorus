// chorus is a single foreground CLI that owns several ACP agent sessions
// (Claude Code, Gemini CLI, opencode) for the life of one terminal
// session. See chorus-spec.md for the full design.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	acp "github.com/coder/acp-go-sdk"

	"chorus/internal/bus"
	"chorus/internal/delegate"
	"chorus/internal/policy"
	"chorus/internal/registry"
	"chorus/internal/render"
	"chorus/internal/session"
	"chorus/internal/sessionstore"
	"chorus/internal/tui"
)

func main() {
	// chorus re-invokes itself as the delegate-mcp subprocess (§11) — see
	// internal/delegate's package doc for why this has to be a separate
	// process rather than something chorus's main loop just handles
	// in-process.
	if len(os.Args) > 1 && os.Args[1] == "__mcp_delegate" {
		if err := delegate.RunMCPServer(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "chorus (delegate-mcp):", err)
			os.Exit(1)
		}
		return
	}

	fresh := hasFlag(os.Args[1:], "--fresh")

	if err := run(fresh); err != nil {
		fmt.Fprintln(os.Stderr, "chorus:", err)
		os.Exit(1)
	}
}

func run(fresh bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	agentSpecs, err := registry.Load(filepath.Join(cwd, "agents.yaml"))
	if err != nil {
		return fmt.Errorf("load agents.yaml: %w", err)
	}

	cfg, err := policy.Load(filepath.Join(cwd, "policy.yaml"))
	if err != nil {
		return fmt.Errorf("load policy.yaml: %w", err)
	}

	store, err := sessionstore.Load(filepath.Join(cwd, ".chorus", "sessions.json"))
	if err != nil {
		return fmt.Errorf("load .chorus/sessions.json: %w", err)
	}

	outputCh := make(chan bus.Update, 64)
	permCh := make(chan bus.PermissionRequest, 4)
	errCh := make(chan tui.ErrMsg, 8)
	delegateLogCh := make(chan delegate.LogEntry, 8)

	// logDir holds each agent subprocess's raw stderr — deliberately never
	// the live terminal (see session.Connect's doc comment): an agent CLI
	// writing an unexpected line to its own stderr would otherwise land
	// raw on the same screen bubbletea is actively redrawing, uncoordinated
	// with chorus's own rendering. A directory-creation failure degrades
	// to discarding subprocess stderr entirely (io.Discard) rather than
	// falling back to the terminal, which would reintroduce exactly the
	// problem this exists to avoid.
	logDir := filepath.Join(cwd, ".chorus", "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't create %s, agent subprocess stderr will be discarded (not shown, not logged): %v\n", logDir, err)
		logDir = ""
	}

	// Phase 1: connect to every registered agent (subprocess + ACP
	// initialize handshake only — no session yet). Split from session
	// creation because the delegate-mcp Hub (phase 2) needs to know the
	// final set of successfully connected agents before any of them gets
	// a session with the delegate tool attached.
	conns := make(map[string]*session.Connection)
	var startErrs []string
	for _, spec := range agentSpecs {
		fmt.Printf("starting %s (%s %s)...\n", spec.Name, spec.Command, strings.Join(spec.Args, " "))
		conn, err := session.Connect(ctx, spec, outputCh, permCh, cfg.Agents, agentStderr(logDir, spec.Name))
		if err != nil {
			startErrs = append(startErrs, fmt.Sprintf("%s: %v", spec.Name, err))
			continue
		}
		conns[spec.Name] = conn
	}
	for _, e := range startErrs {
		fmt.Fprintln(os.Stderr, "warning: failed to start", e)
	}
	if len(conns) == 0 {
		return fmt.Errorf("no agents started")
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	// Phase 2: start the delegate-mcp callback hub now that the final
	// agent set is known (§11). Delegation is only meaningful with 2+
	// agents; with exactly one, no mcpServers are attached anywhere and
	// the hub simply never receives a call.
	coll := delegate.NewCollectors()
	hub, err := delegate.NewHub(cwd, coll, delegateLogCh)
	if err != nil {
		return fmt.Errorf("start delegate hub: %w", err)
	}
	defer hub.Close()
	hub.SetConnections(conns)
	attachDelegate := len(conns) > 1

	// Phase 3: create (or resume) each connection's main interactive
	// session, with the delegate tool attached if there's more than one
	// agent to delegate to.
	//
	// Resume via ACP's own session/load (§0/CLAUDE.md): only offered when
	// the agent advertised the 'loadSession' capability at initialize AND
	// .chorus/sessions.json has a session ID for it from a previous run
	// AND --fresh wasn't passed. session/load replays the session's prior
	// history back through the normal session/update -> outputCh -> render
	// path, so a resumed conversation appears as visible scrollback, not
	// silent state. A stale/rejected ID falls back to a fresh session
	// rather than failing startup, and clears the bad ID so future runs
	// don't keep retrying it.
	sessions := make(map[string]*session.AgentSession)
	workers := make(map[string]*tui.AgentWorker)
	for _, spec := range agentSpecs {
		conn, ok := conns[spec.Name]
		if !ok {
			continue
		}
		var mcpServers []acp.McpServer
		if attachDelegate {
			ms, err := delegateMcpServers(hub, agentSpecs, conns, spec.Name)
			if err != nil {
				return fmt.Errorf("build delegate mcpServers for %s: %w", spec.Name, err)
			}
			mcpServers = ms
		}

		s, resumed := resumeOrNewSession(ctx, conn, store, fresh, spec.Name, cwd, mcpServers)
		if s == nil {
			fmt.Fprintf(os.Stderr, "warning: failed to create session for %s\n", spec.Name)
			conn.Close()
			delete(conns, spec.Name)
			continue
		}
		if err := store.Set(spec.Name, string(s.SessionID)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to save session ID for %s: %v\n", spec.Name, err)
		}

		sessions[spec.Name] = s
		workers[spec.Name] = tui.StartWorker(ctx, s, errCh)
		if resumed {
			fmt.Printf("%s resumed (session %s)\n", spec.Name, s.SessionID)
		} else {
			fmt.Printf("%s ready (session %s)\n", spec.Name, s.SessionID)
		}
		// Seed a one-time delegation briefing on brand-new sessions, once
		// delegation is actually possible (2+ agents). Real ACP history
		// replay (session/load) means this becomes part of the agent's
		// own conversation and persists across every future resume — no
		// "resend periodically" logic needed on top of the !resumed
		// check. Sent as a real, visible turn (not hidden like a
		// delegation sub-session) so the user can see exactly what bias
		// was introduced. Known gap, documented in CLAUDE.md: a session
		// resumed from before a 2nd agent existed never gets this —
		// restart with --fresh to pick it up.
		if attachDelegate && !resumed && cfg.BriefingEnabled() {
			roster := delegate.BuildRoster(agentSpecs, conns, spec.Name)
			tui.QueuePrompt(spec.Name, buildBriefingText(roster), workers)
		}
	}
	if len(sessions) == 0 {
		return fmt.Errorf("no agent sessions created")
	}

	r := render.New()
	// Force glamour's markdown style once, now, rather than leaving it on
	// glamour.WithAutoStyle()'s default TTY auto-detection: SetWidth (and
	// therefore this auto-detection) re-runs on every tea.WindowSizeMsg,
	// including the very first one bubbletea sends right as its Program
	// starts — auto-detection queries the terminal (an OSC background-
	// color request/response over stdin/stdout) and could race against
	// bubbletea taking over raw stdin at essentially the same moment.
	// Detecting once here, strictly before tea.NewProgram runs, avoids
	// that race entirely.
	style := "light"
	if lipgloss.HasDarkBackground() {
		style = "dark"
	}
	r.SetStyle(glamour.WithStandardStyle(style))
	imageDir := filepath.Join(cwd, ".chorus", "images")
	if err := os.MkdirAll(imageDir, 0o700); err != nil { // owner-only, see chorus-spec.md §0's 2026-08-22 audit
		fmt.Fprintf(os.Stderr, "warning: couldn't create %s, inbound images will show as a placeholder: %v\n", imageDir, err)
	} else {
		r.WithImageDir(imageDir)
	}

	fmt.Println(`Type "<agent>: <text>" to prompt a specific agent, or just type a prompt to auto-route it. Type "/name ..." to run a known slash command, "commands" to list them, "capabilities" to show what each agent advertises, "thoughts" to toggle agent thinking output, quit/exit to end.`)

	// The rest of chorus's interactive behavior — permission Q&A, route-
	// ambiguity prompts, slash commands, auto-routing, the scrolling
	// output viewport and input box — is internal/tui's Model, driven by
	// bubbletea's own event loop from here on. WithAltScreen takes over
	// the terminal (the "starting X.../X ready" banners above stay
	// visible on the normal screen buffer until then, then are hidden
	// until the program exits — see chorus-spec.md's TUI design notes,
	// this is a deliberate trade-off, not an oversight).
	// Deliberately NOT tea.WithMouseCellMotion(): enabling mouse capture
	// stops the terminal from treating click-drag as native text
	// selection (the mouse events go to chorus instead) — confirmed live
	// to actually break copy/paste, which matters more for a coding tool
	// than scroll-wheel support. PgUp/PgDn/Home/End (Model.handleKey)
	// cover scrolling without this trade-off; Claude Code's own CLI makes
	// the same call.
	p := tea.NewProgram(tui.New(tui.Config{
		Ctx:           ctx,
		Renderer:      r,
		Collectors:    coll,
		Workers:       workers,
		Routing:       cfg.Routing,
		AgentSpecs:    agentSpecs,
		Conns:         conns,
		OutputCh:      outputCh,
		PermCh:        permCh,
		ErrCh:         errCh,
		DelegateLogCh: delegateLogCh,
	}), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// resumeOrNewSession tries session/load when possible, falling back to a
// fresh session/new — on missing capability, missing --fresh override,
// no saved ID, or a rejected load. Returns (nil, false) only if even the
// fresh-session fallback fails. resumed reports which path was taken.
func resumeOrNewSession(ctx context.Context, conn *session.Connection, store *sessionstore.Store, fresh bool, agent, cwd string, mcpServers []acp.McpServer) (s *session.AgentSession, resumed bool) {
	if !fresh && conn.SupportsLoadSession {
		if savedID, ok := store.Get(agent); ok {
			s, err := conn.LoadSession(ctx, cwd, acp.SessionId(savedID), mcpServers)
			if err == nil {
				return s, true
			}
			fmt.Fprintf(os.Stderr, "warning: %s: couldn't resume saved session (%v) — starting fresh\n", agent, err)
			if clearErr := store.Clear(agent); clearErr != nil {
				fmt.Fprintf(os.Stderr, "warning: %s: failed to clear stale session ID: %v\n", agent, clearErr)
			}
		}
	}
	s, err := conn.NewSession(ctx, cwd, mcpServers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s: session/new: %v\n", agent, err)
		return nil, false
	}
	return s, false
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// agentStderr opens (truncating) a per-agent log file under dir for
// session.Connect's stderr param — see that function's doc comment for
// why this is never os.Stderr directly. dir == "" (logDir creation
// failed) or the file open itself failing both degrade to io.Discard: a
// missing debug log is a much smaller problem than a corrupted TUI, so
// this never falls back to the terminal.
func agentStderr(dir, agent string) io.Writer {
	if dir == "" {
		return io.Discard
	}
	f, err := os.OpenFile(filepath.Join(dir, agent+".stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't open stderr log for %s, its subprocess stderr will be discarded: %v\n", agent, err)
		return io.Discard
	}
	return f
}

// delegateMcpServers builds the mcpServers entry that hands one agent the
// `delegate` tool: chorus re-invoked as `chorus __mcp_delegate`, pointed
// back at hub via environment variables. Source is set to the agent this
// entry is attached to, so the hub's log (and its one-hop depth
// enforcement) knows who initiated a given delegate call. The roster of
// every OTHER connected agent (cost tier + notes) is passed the same way,
// so the spawned subprocess can build a description richer than a bare
// agent-name list (internal/delegate.BuildRoster/EncodeRoster).
func delegateMcpServers(hub *delegate.Hub, agentSpecs []session.Spec, conns map[string]*session.Connection, agentName string) ([]acp.McpServer, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	roster := delegate.BuildRoster(agentSpecs, conns, agentName)
	encodedRoster, err := delegate.EncodeRoster(roster)
	if err != nil {
		return nil, fmt.Errorf("encode delegate roster for %s: %w", agentName, err)
	}
	return []acp.McpServer{
		{Stdio: &acp.McpServerStdio{
			Name:    "chorus-delegate",
			Command: exe,
			Args:    []string{"__mcp_delegate"},
			Env: []acp.EnvVariable{
				{Name: delegate.EnvAddr, Value: hub.Addr()},
				{Name: delegate.EnvToken, Value: hub.Token()},
				{Name: delegate.EnvSource, Value: agentName},
				{Name: delegate.EnvRoster, Value: encodedRoster},
			},
		}},
	}, nil
}

// buildBriefingText builds the one-time seeded message queued on a
// brand-new session once delegation is possible (main.go phase 3): it
// names the other connected agents (via the same roster data the delegate
// tool's own description uses, internal/delegate.BuildRoster) and
// instructs the agent to delegate proactively but treat replies as
// unverified drafts — mirrors the tool description's framing so the two
// surfaces reinforce each other rather than saying different things.
// buildBriefingText's opening two sentences exist specifically because of
// a live-testing finding (chorus-spec.md §0): sent with no self-
// identification, at least one agent (opencode) spent its first turn
// reasoning about whether this was a prompt-injection attempt embedded in
// its instructions, rather than just internalizing it — a reasonable
// reaction, since ACP has no "system" role separate from "user" for
// chorus to send this on, so from the model's point of view it's
// indistinguishable in shape from a user message. Explicitly naming the
// sender (chorus, not the user) and stating no task/response is expected
// is a direct, evidence-based fix for that reaction, not a guess.
func buildBriefingText(roster []delegate.RosterEntry) string {
	var b strings.Builder
	b.WriteString("This message is automated setup text sent by chorus itself — the multi-agent CLI harness you're ")
	b.WriteString("running under — not a message from the user, and not a task. It's sent once, automatically, at the ")
	b.WriteString("start of every session where delegation is available, the same way a tool's system prompt or a ")
	b.WriteString("CLI's startup banner would be. No response or acknowledgment is needed; there is nothing to do ")
	b.WriteString("yet. Just retain this for when a relevant task actually arrives from the user.\n\n")
	b.WriteString("You're one of several AI agents connected through chorus. Other agents available to you right now:\n\n")
	b.WriteString(delegate.FormatRosterLines(roster))
	b.WriteString("\nYou have a `delegate` tool that hands a self-contained sub-task to one of them and returns ")
	b.WriteString("its text reply — it runs in a fresh sub-session with no memory of this conversation, so give it ")
	b.WriteString("full context. When part of what the user asks fits a listed agent's cost tier or notes better ")
	b.WriteString("than doing it yourself, proactively delegate that part instead of doing everything yourself by ")
	b.WriteString("default. Treat a delegate reply as an unverified draft you're responsible for checking, not a final answer.")
	return b.String()
}
