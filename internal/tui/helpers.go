package tui

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"chorus/internal/bus"
	"chorus/internal/delegate"
	"chorus/internal/policy"
	"chorus/internal/render"
	"chorus/internal/router"
	"chorus/internal/session"
)

// AgentWorker serializes prompts to one AgentSession: a line handed to
// dispatch() is queued here rather than calling Prompt inline, because
// Prompt blocks until the agent's whole turn completes and the Model's
// Update loop must stay free to keep handling outputCh/permCh messages
// while that happens (chorus-spec.md §7). Queued as content blocks, not
// plain text, so an @-attached image can ride alongside the text in the
// same turn.
type AgentWorker struct {
	sess *session.AgentSession
	in   chan []acp.ContentBlock
}

// ErrMsg is a per-prompt error tagged with which agent it came from.
type ErrMsg struct {
	Agent string
	Err   error
}

// StartWorker launches the goroutine that drains w.in and calls
// s.PromptContent for each queued prompt, reporting any error on errCh.
// Called from main.go's startup phase 3, before the Model exists — the
// returned worker is handed to New via Config.Workers.
func StartWorker(ctx context.Context, s *session.AgentSession, errCh chan<- ErrMsg) *AgentWorker {
	w := &AgentWorker{sess: s, in: make(chan []acp.ContentBlock, 16)}
	go func() {
		for blocks := range w.in {
			if err := s.PromptContent(ctx, blocks); err != nil {
				errCh <- ErrMsg{Agent: s.Name, Err: err}
			}
		}
	}()
	return w
}

// pendingRoute is a prompt awaiting a user choice of agent — either §9's
// ask_when_ambiguous (candidates == nil: offer every known agent) or an
// ambiguous slash command declared by more than one agent (candidates:
// just those).
type pendingRoute struct {
	text       string
	candidates []string
}

// joinMsgs joins non-empty parts with a newline — used to combine an
// informational line (e.g. "(auto-routed to X)") with a possible error
// from the queuePrompt call that followed it, without a stray blank line
// when the second part is empty (the common case).
func joinMsgs(parts ...string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "\n")
}

// dispatch handles one input line, exactly as chorus-spec.md's REPL
// syntax defines it:
//
//  1. An explicit "<agent>: text" prefix (§9: "explicit override, always
//     wins") queues directly.
//  2. A bare "/name ..." matching a command any connected agent
//     advertised via available_commands_update routes straight to
//     whichever agent(s) declare it. Ambiguous (2+ agents declare the
//     same name) returns a pendingRoute restricted to just those agents.
//  3. Anything else — including a colon that isn't actually an agent
//     prefix, e.g. a colon appearing naturally inside free text —
//     auto-routes via router.Choose.
//
// Returns any informational text to display (may be "") and, if routing
// was ambiguous, a non-nil pendingRoute the caller should prompt for and
// later resolve via queuePrompt once the user answers.
//
// Unlike the line-based REPL this was ported from, dispatch never prints
// directly — bubbletea owns the terminal, so every informational message
// is returned as text for Update() to append as a block instead.
func dispatch(line string, workers map[string]*AgentWorker, routing policy.Routing, commands map[string][]acp.AvailableCommand) (string, *pendingRoute) {
	if agent, rest, ok := strings.Cut(line, ":"); ok {
		agent = strings.TrimSpace(agent)
		rest = strings.TrimSpace(rest)
		// Only treat this as a prefix attempt if the part before ":" has
		// no spaces — "please fix this: it's broken" isn't someone typing
		// an agent name, so let it fall through to auto-routing instead.
		if agent != "" && !strings.ContainsAny(agent, " \t") {
			if rest == "" {
				return fmt.Sprintf("usage: <agent>: <text>  (agents: %s)", strings.Join(agentNames(workers), ", ")), nil
			}
			if !knownAgent(agent, workers) {
				return fmt.Sprintf("unknown agent %q (agents: %s)", agent, strings.Join(agentNames(workers), ", ")), nil
			}
			return queuePrompt(agent, rest, workers), nil
		}
	}

	if strings.HasPrefix(line, "/") {
		if owners := commandOwners(commands, line); len(owners) > 0 {
			if len(owners) == 1 {
				msg := queuePrompt(owners[0], line, workers)
				return joinMsgs(fmt.Sprintf("(/%s -> %s)", commandName(line), owners[0]), msg), nil
			}
			return "", &pendingRoute{text: line, candidates: owners}
		}
		// No agent has advertised this command (yet, or at all) — fall
		// through to ordinary routing below rather than erroring; it
		// might just be prose that happens to start with "/".
	}

	decision := router.Choose(routing, line)
	if !decision.Matched && routing.AskWhenAmbiguous {
		return "", &pendingRoute{text: line}
	}
	if decision.Agent == "" || !knownAgent(decision.Agent, workers) {
		return fmt.Sprintf("no route for this prompt — use \"<agent>: text\" (agents: %s)", strings.Join(agentNames(workers), ", ")), nil
	}
	msg := queuePrompt(decision.Agent, line, workers)
	return joinMsgs(fmt.Sprintf("(auto-routed to %s)", decision.Agent), msg), nil
}

// commandName extracts the command name from a "/name ..." line, without
// the leading slash.
func commandName(line string) string {
	name, _, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	return name
}

// commandOwners returns the sorted names of agents that have advertised a
// command matching line's leading "/name" (case-insensitive), based on
// the most recent available_commands_update each has sent.
func commandOwners(commands map[string][]acp.AvailableCommand, line string) []string {
	name := strings.ToLower(commandName(line))
	if name == "" {
		return nil
	}
	var owners []string
	for agent, cmds := range commands {
		for _, c := range cmds {
			if strings.ToLower(c.Name) == name {
				owners = append(owners, agent)
				break
			}
		}
	}
	sort.Strings(owners)
	return owners
}

// formatCommands lists every agent's currently known slash commands, for
// the "commands" REPL command — the discoverability chorus otherwise
// loses by wrapping each agent's native interface.
func formatCommands(commands map[string][]acp.AvailableCommand) string {
	agents := make([]string, 0, len(commands))
	for a := range commands {
		agents = append(agents, a)
	}
	sort.Strings(agents)

	var b strings.Builder
	if len(agents) == 0 {
		fmt.Fprintln(&b, "no agent has reported any commands yet")
		return b.String()
	}
	for _, a := range agents {
		fmt.Fprintf(&b, "%s:\n", a)
		cmds := commands[a]
		if len(cmds) == 0 {
			fmt.Fprintln(&b, "  (none)")
			continue
		}
		sorted := append([]acp.AvailableCommand(nil), cmds...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, c := range sorted {
			fmt.Fprintf(&b, "  /%-20s %s\n", render.StripANSI(c.Name), render.StripANSI(c.Description))
		}
	}
	return b.String()
}

// formatCapabilities prints each connected agent's advertised ACP
// capabilities that chorus's behavior actually branches on — session
// resume and image prompts. Intended for manually verifying a newly-added
// or newly-working agent against what's already known to work — see
// CLAUDE.md's Gemini verification playbook.
func formatCapabilities(specs []session.Spec, conns map[string]*session.Connection) string {
	var b strings.Builder
	for _, spec := range specs {
		conn, ok := conns[spec.Name]
		if !ok {
			fmt.Fprintf(&b, "%s: not connected\n", spec.Name)
			continue
		}
		fmt.Fprintf(&b, "%s:\n", spec.Name)
		fmt.Fprintf(&b, "  loadSession (resume):  %v\n", conn.SupportsLoadSession)
		fmt.Fprintf(&b, "  promptCapabilities.image: %v\n", conn.SupportsImagePrompts)
	}
	return b.String()
}

// QueuePrompt is queuePrompt, exported for main.go's startup-phase use:
// the delegation briefing is queued before any Model exists (main.go's
// phase 3, alongside session creation), so it can't go through Model's
// own dispatch path. Returns an informational message on failure ("" on
// success).
func QueuePrompt(agent, text string, workers map[string]*AgentWorker) string {
	return queuePrompt(agent, text, workers)
}

// queuePrompt sends text (converted to content blocks) to agent's worker.
// Returns an informational message on failure ("" on success) instead of
// printing directly, since bubbletea owns the terminal.
func queuePrompt(agent, text string, workers map[string]*AgentWorker) string {
	w, ok := workers[agent]
	if !ok {
		return fmt.Sprintf("unknown agent %q", agent)
	}
	select {
	case w.in <- buildPromptBlocks(text):
		return ""
	default:
		return fmt.Sprintf("%s is still processing a backlog of prompts, try again shortly", agent)
	}
}

// buildPromptBlocks turns raw input text into the content-block sequence
// actually sent to the agent: any @path.png-style attachments (see
// extractAttachments) become leading acp.ImageBlocks, followed by a
// single TextBlock with the attachment tokens stripped out. If nothing
// but attachment tokens remain (or extraction found nothing at all), the
// original text is sent verbatim rather than an empty prompt.
func buildPromptBlocks(text string) []acp.ContentBlock {
	clean, images := extractAttachments(text)
	if len(images) == 0 {
		return []acp.ContentBlock{acp.TextBlock(text)}
	}
	blocks := make([]acp.ContentBlock, 0, len(images)+1)
	blocks = append(blocks, images...)
	if clean != "" {
		blocks = append(blocks, acp.TextBlock(clean))
	}
	return blocks
}

// attachmentRE matches an @-prefixed path ending in a common image
// extension, e.g. "@screenshot.png" or "@./docs/before.jpg". Deliberately
// simple (no quoting/escaping support) — this is an input-line
// convenience, not a shell.
var attachmentRE = regexp.MustCompile(`@(\S+\.(?:png|jpe?g|gif|webp))\b`)

// extractAttachments pulls @path.png-style tokens out of text, reading
// and base64-encoding each into an acp.ImageBlock. A token that doesn't
// resolve to a readable file is left in place (so the user sees it wasn't
// consumed) and a warning is printed to stderr, rather than silently
// dropping it or failing the whole prompt.
func extractAttachments(text string) (clean string, images []acp.ContentBlock) {
	clean = attachmentRE.ReplaceAllStringFunc(text, func(m string) string {
		path := m[1:] // strip leading "@"
		data, mime, err := readImageBase64(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: couldn't attach %s: %v\n", path, err)
			return m
		}
		images = append(images, acp.ImageBlock(data, mime))
		return ""
	})
	return strings.TrimSpace(clean), images
}

func readImageBase64(path string) (data, mime string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(b), mimeForImageExt(filepath.Ext(path)), nil
}

func mimeForImageExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// extractAgentText returns an update's agent_message_chunk text, if it
// has one. Only this update kind counts toward a delegation call's
// collected result — agent_thought_chunk and others are hidden from the
// interactive terminal when they belong to a sub-session but don't
// contribute reply text.
func extractAgentText(u bus.Update) (string, bool) {
	chunk := u.Notification.Update.AgentMessageChunk
	if chunk == nil || chunk.Content.Text == nil {
		return "", false
	}
	return chunk.Content.Text.Text, true
}

func formatDelegateLog(le delegate.LogEntry) string {
	task := le.Task
	if len(task) > 80 {
		task = task[:80] + "..."
	}
	status := "ok"
	if le.Err != nil {
		// StripANSI here even though this is normally a Go-level/protocol
		// error, not agent-authored text — see formatErrLine's comment
		// (internal/tui/model.go) for the same reasoning: a delegated
		// call's error can in principle echo back agent/subprocess-
		// controlled content, and sanitizing it costs nothing.
		status = "error: " + render.StripANSI(le.Err.Error())
	}
	return fmt.Sprintf("\n[delegate] %s -> %s: %q (%s, %s)\n", le.Source, le.Target, task, le.Duration.Round(time.Millisecond), status)
}

// formatRouteAskPrompt renders the question shown while a pendingRoute is
// awaiting an answer — appended as a block, not printed, so it stays
// visible in the scrollable document once answered.
func formatRouteAskPrompt(ask *pendingRoute, workers map[string]*AgentWorker) string {
	names := ask.candidates
	question := "no routing rule matched — which agent should handle this?"
	if names == nil {
		names = agentNames(workers)
	} else {
		question = fmt.Sprintf("more than one agent has a %q command — which did you mean?", "/"+commandName(ask.text))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n", question)
	for i, n := range names {
		fmt.Fprintf(&b, "  %d) %s\n", i+1, n)
	}
	return b.String()
}

// matchName resolves line against names by 1-based index or
// case-insensitive exact match.
func matchName(line string, names []string) (string, bool) {
	if idx, err := strconv.Atoi(line); err == nil {
		if idx >= 1 && idx <= len(names) {
			return names[idx-1], true
		}
		return "", false
	}
	lower := strings.ToLower(line)
	for _, n := range names {
		if strings.ToLower(n) == lower {
			return n, true
		}
	}
	return "", false
}

// answerPermission parses the user's reply to a permission prompt: either
// the 1-based option number or a case-insensitive match on the option's
// name/kind. Returns false (reprompt, don't consume) on an invalid reply.
func answerPermission(pending *bus.PermissionRequest, line string) bool {
	opts := pending.Req.Options
	lower := strings.ToLower(line)

	if idx, err := strconv.Atoi(line); err == nil {
		if idx >= 1 && idx <= len(opts) {
			selectOption(pending, opts[idx-1].OptionId)
			return true
		}
		return false
	}
	for _, o := range opts {
		if strings.ToLower(o.Name) == lower || strings.ToLower(string(o.Kind)) == lower {
			selectOption(pending, o.OptionId)
			return true
		}
	}
	if lower == "c" || lower == "cancel" {
		pending.Resp <- acp.RequestPermissionResponse{
			Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}},
		}
		return true
	}
	return false
}

func selectOption(pending *bus.PermissionRequest, id acp.PermissionOptionId) {
	pending.Resp <- acp.RequestPermissionResponse{
		Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: id}},
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func knownAgent(name string, workers map[string]*AgentWorker) bool {
	_, ok := workers[name]
	return ok
}

func agentNames(workers map[string]*AgentWorker) []string {
	names := make([]string, 0, len(workers))
	for n := range workers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
