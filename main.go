// chorus is a single foreground CLI that owns several ACP agent sessions
// (Claude Code, Gemini CLI, opencode) for the life of one terminal
// session. See chorus-spec.md for the full design.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

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

	agentSpecs, cfg, err := loadAgentConfig(cwd)
	if err != nil {
		return err
	}
	if err := validateRoutingConfig(agentSpecs, cfg.Routing); err != nil {
		return err
	}

	store, err := sessionstore.Load(filepath.Join(cwd, ".chorus", "sessions.json"))
	if err != nil {
		return fmt.Errorf("load .chorus/sessions.json: %w", err)
	}

	outputCh := make(chan bus.Update, 64)
	permCh := make(chan bus.PermissionRequest, 4)
	errCh := make(chan tui.ErrMsg, 8)
	doneCh := make(chan tui.PromptDoneMsg, 8)
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
	// costTiers feeds acpclient.Client's delegation-nudge logic (CLAUDE.md's
	// delegation-maximization plan item 2) — built once, upfront, from the
	// full registry (unlike idle-checking, which needs tui.AgentWorker
	// instances that only exist later, in phase 3 below).
	costTiers := make(map[string]string, len(agentSpecs))
	for _, spec := range agentSpecs {
		costTiers[spec.Name] = spec.CostTier
	}

	conns := make(map[string]*session.Connection)
	var startErrs []string
	for _, spec := range agentSpecs {
		fmt.Printf("starting %s (%s %s)...\n", spec.Name, spec.Command, strings.Join(spec.Args, " "))
		conn, err := session.Connect(ctx, spec, outputCh, permCh, cfg.Agents, cfg.Delegation, costTiers, agentStderr(logDir, spec.Name))
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
	// Delegation is now opt-in (agents.yaml's delegation.enabled, default
	// false — chorus-spec.md §0): the `delegate` MCP tool's schema costs
	// ~500-1400+ tokens on EVERY turn of EVERY agent it's attached to,
	// whether ever used or not, and live use showed agents rarely call it
	// on their own. attachDelegate stays the single all-or-nothing gate
	// for the whole run (mcpServers is a one-time NewSession/LoadSession
	// construction param, not mutable later) — it now also requires the
	// config opt-in, not just "2+ agents connected."
	attachDelegate := len(conns) > 1 && cfg.Delegation.EnabledOrDefault()

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
	// workersMu guards every access to workers below. The main goroutine
	// keeps writing new entries into it as later agents in agentSpecs
	// finish starting, while the SetIdleChecker/SetNudgeFunc closures wired
	// up per-iteration below can be invoked from a DIFFERENT agent's
	// connection read-loop goroutine as soon as that agent's worker starts
	// processing its queued briefing — i.e. concurrently with this loop
	// still running for later agents. Without this lock that's an
	// unsynchronized concurrent map read+write, which is a fatal Go runtime
	// error, not a recoverable one.
	var workersMu sync.Mutex
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
		if !resumed {
			// A genuinely fresh session's history is empty — whatever
			// briefing status a PRIOR session under this agent name reached
			// (e.g. before a stale ID was rejected, or before --fresh) no
			// longer applies. See ResetBriefed's doc comment.
			if err := store.ResetBriefed(spec.Name); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to reset delegation-briefing status for %s: %v\n", spec.Name, err)
			}
		}

		sessions[spec.Name] = s
		workersMu.Lock()
		workers[spec.Name] = tui.StartWorker(ctx, s, errCh, doneCh)
		workersMu.Unlock()
		// Wire up the delegation-nudge callbacks now that this agent's
		// worker exists (CLAUDE.md's delegation-maximization plan item 2)
		// — ONLY when delegation is actually attached. Found during this
		// same change's own review: this used to be unconditional, so a
		// project that set delegation.enabled: false but left `prefer`
		// populated would still nudge a metered agent to "use your
		// delegate tool" — a tool that, under the new gating, was never
		// attached to its session at all. Both closures capture the
		// `workers` map by reference, not by value — later iterations of
		// this loop still populate entries these closures will see
		// correctly whenever they're actually invoked (well after
		// startup, from a connection's own read-loop goroutine), even
		// though not every agent's worker exists yet at the moment the
		// closures are created. Guarded by workersMu since these run
		// concurrently with this loop's own writes — see its declaration
		// above.
		if attachDelegate {
			conn.SetIdleChecker(func(name string) bool {
				workersMu.Lock()
				w, ok := workers[name]
				workersMu.Unlock()
				return ok && w.Idle()
			})
			conn.SetNudgeFunc(func(agent, text string) {
				workersMu.Lock()
				defer workersMu.Unlock()
				tui.QueuePrompt(agent, text, workers)
			})
		}
		if resumed {
			fmt.Printf("%s resumed (session %s)\n", spec.Name, s.SessionID)
		} else {
			fmt.Printf("%s ready (session %s)\n", spec.Name, s.SessionID)
		}
		// Seed a one-time delegation briefing once delegation is actually
		// possible (2+ agents) AND this agent's session hasn't already
		// received one — tracked persistently in sessionstore
		// (Store.Briefed/MarkBriefed), independent of whether THIS run
		// resumed or started fresh. Real ACP history replay (session/load)
		// means this becomes part of the agent's own conversation and
		// persists across every future resume — no "resend periodically"
		// logic needed once sent. Sent as a real, visible turn (not hidden
		// like a delegation sub-session) so the user can see exactly what
		// bias was introduced.
		//
		// This used to be gated on plain `!resumed`, which meant a session
		// that existed before a 2nd agent ever connected — the ordinary
		// shape of a chorus session that's been running a while — would
		// never receive it no matter how many later runs happened with
		// delegation fully possible (CLAUDE.md's delegation-maximization
		// plan flagged this as a known, deliberately deferred gap). Tracking
		// "briefed" as its own fact fixes that: it now fires on the very
		// next run where attachDelegate is true, for any session that
		// hasn't seen it yet, resumed or not.
		if attachDelegate && !store.Briefed(spec.Name) && cfg.Delegation.BriefingEnabled() {
			roster := delegate.BuildRoster(agentSpecs, conns, spec.Name)
			tui.QueuePrompt(spec.Name, buildBriefingText(roster), workers)
			if err := store.MarkBriefed(spec.Name); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to record delegation briefing for %s: %v\n", spec.Name, err)
			}
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

	fmt.Println(`Type "<agent>: <text>" to prompt a specific agent, or just type a prompt to auto-route it. Type "/name ..." to run a known slash command, "commands" to list them, "capabilities" to show what each agent advertises, "stats" to show direct-vs-delegated tool activity, "thoughts" to toggle agent thinking output, quit/exit to end.`)

	// The rest of chorus's interactive behavior — permission Q&A, route-
	// ambiguity prompts, slash commands, auto-routing, the scrolling
	// output viewport and input box — is internal/tui's Model, driven by
	// bubbletea's own event loop from here on. WithAltScreen takes over
	// the terminal (the "starting X.../X ready" banners above stay
	// visible on the normal screen buffer until then, then are hidden
	// until the program exits — see chorus-spec.md's TUI design notes,
	// this is a deliberate trade-off, not an oversight).
	// tea.WithMouseCellMotion() enables mouse wheel scrolling
	// (Model.Update's tea.MouseMsg case, forwarded to viewport.Update).
	// This does take over plain click-drag (it goes to chorus, not the
	// terminal) — confirmed live this breaks native text selection with
	// no workaround on at least one Windows console. Re-enabled anyway
	// (2026-08, reverting an earlier removal) once it became clear most
	// terminal emulators (confirmed: Windows Terminal) let a modifier key
	// (commonly Shift) held during click-drag override a program's mouse
	// capture and select text natively regardless — the same mechanism
	// Claude Code/Gemini CLI's own UIs rely on, not evidence they avoid
	// mouse capture altogether. Whether the SAME override works in
	// classic cmd.exe/conhost specifically (vs. Windows Terminal) is
	// unconfirmed — if selection turns out to be unrecoverable on a given
	// terminal, that terminal's own mouse-mode override is the thing to
	// chase, not another removal of this line.
	p := tea.NewProgram(tui.New(tui.Config{
		Ctx:           ctx,
		Renderer:      r,
		Collectors:    coll,
		Workers:       workers,
		DefaultAgent:  cfg.DefaultAgent,
		Routing:       cfg.Routing,
		Compaction:    cfg.Compaction,
		Cwd:           cwd,
		AgentSpecs:    agentSpecs,
		Conns:         conns,
		OutputCh:      outputCh,
		PermCh:        permCh,
		ErrCh:         errCh,
		DoneCh:        doneCh,
		DelegateLogCh: delegateLogCh,
	}), tea.WithAltScreen(), tea.WithMouseCellMotion())
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

// embeddedAgentsYAML is this repo's own agents.yaml, baked into the
// binary at build time — chorus's reference config (claude/gemini/
// opencode, permissions/delegation/compaction/routing tuned for them,
// per CLAUDE.md's delegation-maximization plan) doubles as the shipped
// default, rather than maintaining a second, separate "default" file
// that could drift from what's actually tested. loadAgentConfig falls
// back to this only when no local agents.yaml exists in cwd.
//
// policy.yaml no longer exists (chorus-spec.md §0) — permission settings
// (auto_allow/auto_allow_tools) moved into each agent's own entry in this
// one file, so there's exactly one config file to resolve, not two, and
// the earlier "local agents.yaml requires local policy.yaml" guard is
// gone along with the second file it existed to protect.
//
//go:embed agents.yaml
var embeddedAgentsYAML []byte

// loadAgentConfig resolves agents.yaml for this run. A local file in cwd
// always takes precedence over the embedded default.
func loadAgentConfig(cwd string) ([]session.Spec, policy.Config, error) {
	agentsPath := filepath.Join(cwd, "agents.yaml")

	localAgents, err := fileExists(agentsPath)
	if err != nil {
		return nil, policy.Config{}, fmt.Errorf("check %s: %w", agentsPath, err)
	}

	agentsYAML := embeddedAgentsYAML
	if localAgents {
		b, err := os.ReadFile(agentsPath)
		if err != nil {
			return nil, policy.Config{}, fmt.Errorf("read %s: %w", agentsPath, err)
		}
		agentsYAML = b
	}
	agentSpecs, cfg, err := registry.Parse(agentsYAML)
	if err != nil {
		return nil, policy.Config{}, fmt.Errorf("load agents.yaml: %w", err)
	}

	return agentSpecs, cfg, nil
}

// validateRoutingConfig catches a routing.decision_agent misconfiguration
// before spending any time connecting agent subprocesses, rather than
// letting it surface later as a router.Decide fallback path buried in the
// TUI. Only checked when routing.mode is actually "llm" — an unset or
// unknown decision_agent under "off" mode is simply never read. A
// metered decision_agent isn't rejected outright (it's still a valid,
// working configuration), just warned about: routing to a metered agent
// to decide routing for OTHER agents defeats a good chunk of the point.
func validateRoutingConfig(specs []session.Spec, routing policy.Routing) error {
	if routing.ModeOrDefault() != policy.RoutingLLM {
		return nil
	}
	for _, s := range specs {
		if s.Name == routing.DecisionAgent {
			if s.CostTier == "metered" {
				fmt.Fprintf(os.Stderr, "warning: routing.decision_agent %q is cost_tier \"metered\" — LLM-based routing is meant to use a non-metered agent to decide, or it partly defeats its own purpose\n", routing.DecisionAgent)
			}
			return nil
		}
	}
	return fmt.Errorf("routing.mode is \"llm\" but decision_agent %q doesn't match any agent in agents.yaml", routing.DecisionAgent)
}

// fileExists distinguishes "doesn't exist" (fine, fall back to the
// embedded default) from a real stat error (permissions, a bad path
// component, etc. — surfaced rather than silently treated as "missing").
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
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
	b.WriteString("full context. Delegate isn't just for analysis-shaped work (summarize/explain/review) — once ")
	b.WriteString("you've done the judgment part of a task (deciding WHAT needs to change, and why) the mechanical ")
	b.WriteString("part of actually making a well-specified change often doesn't need your own context anymore, ")
	b.WriteString("only the spec you just worked out. That part is delegable too: hand a cheaper agent the exact ")
	b.WriteString("file, the exact change, and the reason, then verify what comes back, instead of applying it ")
	b.WriteString("yourself by default. Proactively look for this split — a task that's \"figure out X, then do X\" ")
	b.WriteString("is rarely one indivisible unit of work. Treat a delegate reply as an unverified draft you're ")
	b.WriteString("responsible for checking, not a final answer — that's true whether you delegated analysis or an edit.")
	return b.String()
}
