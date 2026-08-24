// Package policy implements the §5 permission allow-list: tool calls whose
// ACP ToolKind is in an agent's auto_allow list are answered immediately
// without interrupting the user; everything else (including unlisted
// kinds) falls through to the permission queue. It also holds §9's
// routing config, since the spec's §5 and §9 mockups are both labeled
// "# policy.yaml" — one merged file, two concerns.
//
// Deviation from the spec's policy.yaml mockup, noted deliberately per §0's
// priority order (correctness of the ACP integration > matching the spec
// exactly): the mockup keyed auto_allow/ask on tool *names* like "Read" or
// "Bash", which are internal to each agent and not exposed over ACP. What
// ACP actually gives every agent, uniformly, is ToolCallUpdate.Kind (read,
// edit, delete, move, search, execute, think, fetch, switch_mode, other) —
// so policy.yaml matches on that instead.
package policy

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentPolicy is one agent's section of policy.yaml.
type AgentPolicy struct {
	AutoAllow []string `yaml:"auto_allow"`
	// AutoAllowTools matches a tool call's human-readable Title (case-
	// insensitive prefix — see AutoAllowTool's doc comment for why prefix,
	// not substring), for tools that need naming specifically because
	// their ACP Kind alone can't distinguish them — chorus's own
	// `delegate` MCP tool (§11) is generic "other" kind like any external
	// MCP tool, so Kind-based matching can't single it out from other,
	// genuinely risky "other"-kind tools.
	AutoAllowTools []string `yaml:"auto_allow_tools"`
}

// Policy maps agent name -> its policy.
type Policy map[string]AgentPolicy

// Rule is one §9 routing rule: if any of Match's keywords (case-insensitive
// substring match) appears in the prompt text, Agent handles it.
type Rule struct {
	Match []string `yaml:"match"`
	Agent string   `yaml:"agent"`
}

// Routing is §9's auto-routing config. v1's router is deliberately
// rule-based, not model-based — a classifier call would itself burn
// tokens on a metered agent, defeating the point.
type Routing struct {
	Default          string `yaml:"default"`
	AskWhenAmbiguous bool   `yaml:"ask_when_ambiguous"`
	Rules            []Rule `yaml:"rules"`
}

// Delegation is optional top-level policy.yaml config for the seeded
// first-turn delegation briefing (main.go, chorus-spec.md §11). Missing
// entirely, or with Briefing omitted, defaults to enabled — opt-out, not
// opt-in, since the whole point of the briefing is that delegation
// shouldn't depend on the user remembering to ask for it every time.
type Delegation struct {
	Briefing *bool `yaml:"briefing"`
}

// Config is policy.yaml's fully parsed content.
type Config struct {
	Agents     Policy
	Routing    Routing
	Delegation Delegation
}

// BriefingEnabled reports whether the seeded first-turn delegation
// briefing should fire. Defaults to true when unset.
func (c Config) BriefingEnabled() bool {
	return c.Delegation.Briefing == nil || *c.Delegation.Briefing
}

// Load reads policy.yaml at path. A missing file is not an error — it
// yields an empty Config: every tool call falls through to the permission
// queue (safe default) and routing has no rules or default, so dispatch
// falls back to requiring an explicit agent prefix.
//
// The file's top-level keys are a mix of per-agent policy blocks and one
// "routing" block. yaml.v3 can't decode that directly into one typed
// struct (a fixed field for "routing" plus an open-ended map for
// "everything else" isn't expressible), so this decodes to
// map[string]yaml.Node first and dispatches each key's node to the right
// type.
func Load(path string) (Config, error) {
	cfg := Config{Agents: Policy{}}

	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, err
	}

	var raw map[string]yaml.Node
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return Config{}, fmt.Errorf("policy.yaml: %w", err)
	}

	for key, node := range raw {
		node := node
		if key == "routing" {
			if err := node.Decode(&cfg.Routing); err != nil {
				return Config{}, fmt.Errorf("policy.yaml: routing: %w", err)
			}
			continue
		}
		if key == "delegation" {
			if err := node.Decode(&cfg.Delegation); err != nil {
				return Config{}, fmt.Errorf("policy.yaml: delegation: %w", err)
			}
			continue
		}
		var ap AgentPolicy
		if err := node.Decode(&ap); err != nil {
			return Config{}, fmt.Errorf("policy.yaml: %s: %w", key, err)
		}
		cfg.Agents[key] = ap
	}
	return cfg, nil
}

// AutoAllow reports whether toolKind should be auto-approved for agent
// without asking the user. Unknown agents and unlisted kinds default to
// false (ask), matching §5.
func (p Policy) AutoAllow(agent, toolKind string) bool {
	if toolKind == "" {
		return false
	}
	ap, ok := p[agent]
	if !ok {
		return false
	}
	for _, k := range ap.AutoAllow {
		if k == toolKind {
			return true
		}
	}
	return false
}

// AutoAllowTool reports whether a tool call's title should be
// auto-approved regardless of Kind, per AutoAllowTools. Matching is a
// case-insensitive PREFIX check, not substring — this was tightened
// (chorus-spec.md §0's 2026-08-22 audit) after realizing a substring
// check lets any tool call whose agent-supplied Title merely *contains*
// an allowed word bypass its real permission requirement, e.g. an
// execute-kind Bash call titled "please delegate this" would have
// matched "delegate" and skipped the prompt entirely, regardless of
// what it actually does. Prefix matching against the real MCP naming
// convention (agents observed live titling delegate calls literally
// "mcp__chorus-delegate__delegate") raises the bar: a spoofed title has
// to deliberately open with that exact MCP-server-qualified prefix, not
// just mention the word somewhere in a sentence.
func (p Policy) AutoAllowTool(agent, title string) bool {
	if title == "" {
		return false
	}
	ap, ok := p[agent]
	if !ok {
		return false
	}
	lower := strings.ToLower(title)
	for _, t := range ap.AutoAllowTools {
		if t != "" && strings.HasPrefix(lower, strings.ToLower(t)) {
			return true
		}
	}
	return false
}
