package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RunMCPServer runs chorus in delegate-mcp mode: an MCP stdio server
// exposing one tool, `delegate`, that calls back into the main chorus
// process's Hub over loopback HTTP. This is what each agent's
// mcpServers.stdio config spawns — a grandchild of chorus's own process,
// launched by the agent CLI (claude-agent-acp/gemini/opencode) itself,
// not by chorus directly (see the package doc in hub.go).
func RunMCPServer(ctx context.Context) error {
	addr := os.Getenv(EnvAddr)
	token := os.Getenv(EnvToken)
	source := os.Getenv(EnvSource)
	if addr == "" || token == "" || source == "" {
		return fmt.Errorf("delegate-mcp: missing %s/%s/%s in environment — this must be spawned by chorus's own mcpServers config, not run directly", EnvAddr, EnvToken, EnvSource)
	}

	// EnvRoster is best-effort: a decode failure degrades the tool's
	// description to the generic fallback rather than failing delegation
	// itself (see buildToolDescription).
	roster, err := DecodeRoster(os.Getenv(EnvRoster))
	if err != nil {
		fmt.Fprintf(os.Stderr, "chorus (delegate-mcp): couldn't parse %s, using generic tool description: %v\n", EnvRoster, err)
		roster = nil
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "chorus-delegate"}, nil)

	type args struct {
		Agent string `json:"agent" jsonschema:"which chorus agent to delegate to (e.g. claude, gemini, opencode) — not yourself"`
		Task  string `json:"task" jsonschema:"the self-contained task or question to hand off; the target agent has no memory of this conversation"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "delegate",
		Description: buildToolDescription(roster),
	}, func(ctx context.Context, req *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
		text, err := callHub(ctx, addr, token, source, a.Agent, a.Task)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			}, nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil, nil
	})

	return server.Run(ctx, &mcp.StdioTransport{})
}

// buildToolDescription combines the tool's functional explanation with the
// roster (other connected agents' cost tier + notes, so the calling model
// has concrete signal instead of a bare name list) and a verification
// framing that's always present regardless of roster data — this is what
// makes proactive delegation safe to encourage: the calling agent stays
// responsible for checking a delegate reply, not trusting it outright.
func buildToolDescription(roster []RosterEntry) string {
	var b strings.Builder
	b.WriteString("Delegate a sub-task to another chorus agent and get back its text reply. ")
	b.WriteString("Runs in a fresh, isolated sub-session for the target agent, not its main conversation — ")
	b.WriteString("give it full context in the task text since it starts with none. One hop only: this tool ")
	b.WriteString("is not available from within a delegated sub-session, so the target agent cannot delegate further.")

	if len(roster) > 0 {
		b.WriteString("\n\nAgents available to delegate to:\n")
		b.WriteString(FormatRosterLines(roster))
		b.WriteString("\nWeigh cost tier and each agent's notes against the sub-task before choosing — a cheaper-tier ")
		b.WriteString("agent that fits the task's profile is usually a better choice than defaulting to the most capable one for everything.")
	}

	b.WriteString("\n\nThe reply you get back is an unverified draft, not a final answer: the target agent has no way ")
	b.WriteString("to check its own output against your standards or the rest of this conversation. You remain ")
	b.WriteString("responsible for reviewing it — check it against the task before relying on it or passing it on.")
	return b.String()
}

func callHub(ctx context.Context, addr, token, source, agent, task string) (string, error) {
	body, err := json.Marshal(delegateRequest{Source: source, Agent: agent, Task: task})
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/delegate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("delegate-mcp: calling chorus: %w", err)
	}
	defer resp.Body.Close()

	var dr delegateResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return "", fmt.Errorf("delegate-mcp: decoding chorus's response: %w", err)
	}
	if dr.Error != "" {
		return "", fmt.Errorf("%s", dr.Error)
	}
	return dr.Result, nil
}
