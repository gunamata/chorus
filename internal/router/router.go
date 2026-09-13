// Package router implements the LLM-based routing decision: which agent
// (and optionally which model) should handle a prompt with no explicit
// "<agent>: " prefix, when agents.yaml's routing.mode is "llm". Replaces
// v1's keyword-matching Choose/Rule entirely (chorus-spec.md §0, this
// session's routing overhaul) — a classifier call was originally rejected
// as "burns tokens on a metered agent, defeating the point of routing,"
// but the decision call here is deliberately made by a non-metered
// DecisionAgent instead, at the cost of one extra turn's latency per
// unprefixed prompt.
//
// ACP has no structured/JSON-schema-constrained output for a normal
// prompt turn — chorus only ever gets back streamed plain text. Getting
// a "consistent, structured format" out of an arbitrary agent CLI means
// asking it to reply in a specific JSON shape (BuildDecisionPrompt) and
// leniently parsing whatever text comes back (ParseDecision), tolerant of
// markdown fences and leading prose. This is inherently best-effort:
// ParseDecision returns an error on anything it can't make sense of, and
// callers must always fall back gracefully (chorus's own default agent)
// rather than blocking or failing the user's actual prompt on a
// malformed decision.
package router

import (
	"encoding/json"
	"fmt"
	"strings"

	"chorus/internal/session"
)

// Decision is the routing decision for one prompt.
type Decision struct {
	Agent  string
	Model  string // "" means "just route to Agent, don't switch its model"
	Reason string
}

// DecisionAgentInfo is what BuildDecisionPrompt tells the decision agent
// about one candidate agent — deliberately a small, printable subset of
// registry data, not the whole session.Spec.
type DecisionAgentInfo struct {
	Name     string
	CostTier string
	Notes    string
	Models   []session.ModelInfo
}

// BuildDecisionPrompt builds the text sent to the decision agent's hidden
// sub-session. Opens with the same explicit self-identification framing
// buildBriefingText/buildNudgeText (main.go/internal/acpclient) already
// use — proven necessary live (chorus-spec.md §0: at least one agent
// otherwise suspects a plain instruction message is a prompt-injection
// attempt) — and explicitly instructs the agent not to use tools or
// attempt the task itself, since a hidden decision sub-session still has
// the agent's own built-in tools available even with chorus's MCP servers
// detached.
//
// anonymizeText, when true (policy.Routing.AnonymizeOrDefault — true
// unless the caller's config explicitly disables it), redacts contextText
// and userPrompt via anonymize() before they're embedded — the decision
// agent is picked for routing cost/capability reasons, not necessarily
// one the user would otherwise trust with the raw prompt. Never applied
// to the agents slice (operator-authored agents.yaml config, not user
// data).
func BuildDecisionPrompt(agents []DecisionAgentInfo, defaultAgent, contextText, userPrompt string, anonymizeText bool) string {
	if anonymizeText {
		contextText = anonymize(contextText)
		userPrompt = anonymize(userPrompt)
	}
	var b strings.Builder
	b.WriteString("This is an automated routing-decision request from chorus itself, the multi-agent CLI harness " +
		"you're running under — not a message from the user, and not a task for you to perform. Your only job " +
		"right now is to decide which connected agent (and optionally which of its models) should handle the " +
		"user's next prompt, shown at the end of this message. Do not use any tools, do not investigate the " +
		"codebase, do not attempt the task yourself — just decide who should.\n\n")

	b.WriteString("Agents available to route to:\n\n")
	for _, a := range agents {
		fmt.Fprintf(&b, "- %s", a.Name)
		if a.CostTier != "" {
			fmt.Fprintf(&b, " (cost tier: %s)", a.CostTier)
		}
		if a.Notes != "" {
			fmt.Fprintf(&b, " — %s", a.Notes)
		}
		b.WriteString("\n")
		for _, m := range a.Models {
			fmt.Fprintf(&b, "  - model %q", m.ID)
			if m.Label != "" {
				fmt.Fprintf(&b, " (%s)", m.Label)
			}
			if m.Capabilities != "" {
				fmt.Fprintf(&b, ": %s", m.Capabilities)
			}
			if m.WhenToUse != "" {
				fmt.Fprintf(&b, " — use when: %s", m.WhenToUse)
			}
			b.WriteString("\n")
		}
	}

	if contextText != "" {
		b.WriteString("\nRecent activity on this task so far, for context:\n\n")
		b.WriteString(contextText)
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "\nDefault agent if you're unsure: %s.\n\n", defaultAgent)
	b.WriteString("Reply with ONLY a single JSON object, no markdown code fence, no other text before or after " +
		"it, in exactly this shape:\n")
	b.WriteString(`{"agent": "<agent name>", "model": "<model id, or empty string if not switching models>", "reason": "<one short sentence>"}` + "\n\n")
	b.WriteString("The user's next prompt to route:\n\n")
	b.WriteString(userPrompt)
	return b.String()
}

// decisionJSON is the wire shape ParseDecision expects from the decision
// agent's reply.
type decisionJSON struct {
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

// ParseDecision extracts a Decision from raw — the decision agent's
// concatenated text reply. Tolerates a ```json fenced block and/or
// leading prose the agent added despite being asked not to, by locating
// the first balanced {...} object in the text (brace-matching, respecting
// quoted strings, rather than requiring the whole reply to be valid
// JSON) instead of requiring an exact match. Returns an error (never
// panics) if no JSON object can be found, it doesn't unmarshal, or
// `agent`/`model` don't validate against the known set — callers must
// treat any error as "fall back to the default agent," not a fatal
// condition.
func ParseDecision(raw string, agents []DecisionAgentInfo) (Decision, error) {
	obj, ok := extractJSONObject(raw)
	if !ok {
		return Decision{}, fmt.Errorf("no JSON object found in decision reply: %q", truncate(raw, 200))
	}
	var dj decisionJSON
	if err := json.Unmarshal([]byte(obj), &dj); err != nil {
		return Decision{}, fmt.Errorf("decision reply wasn't valid JSON: %w", err)
	}
	info, ok := findAgent(agents, dj.Agent)
	if !ok {
		return Decision{}, fmt.Errorf("decision named unknown agent %q", dj.Agent)
	}
	if dj.Model != "" && !hasModel(info.Models, dj.Model) {
		return Decision{}, fmt.Errorf("decision named unknown model %q for agent %q", dj.Model, dj.Agent)
	}
	return Decision{Agent: dj.Agent, Model: dj.Model, Reason: dj.Reason}, nil
}

func findAgent(agents []DecisionAgentInfo, name string) (DecisionAgentInfo, bool) {
	for _, a := range agents {
		if a.Name == name {
			return a, true
		}
	}
	return DecisionAgentInfo{}, false
}

func hasModel(models []session.ModelInfo, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// extractJSONObject finds the first balanced {...} substring in s. Brace-
// matching (not a naive first-{-to-last-} slice, which would mis-bound if
// the agent's reasoning text elsewhere contains braces) and aware of
// quoted strings, so a brace inside a JSON string value doesn't throw off
// the depth count. This also transparently handles a ```json fence around
// the object — the fence characters aren't braces or quotes, so they're
// simply skipped over on the way to finding the real object, no separate
// fence-stripping step needed.
func extractJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start == -1 {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
