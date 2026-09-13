// Package policy holds the typed shape of agents.yaml's non-registry
// settings: the §5 permission allow-list (tool calls whose ACP
// ToolCallUpdate.Kind is in an agent's auto_allow list are answered
// immediately without interrupting the user; everything else falls
// through to the permission queue), delegation config, compaction config,
// and routing config. All of it is decoded by internal/registry.Parse —
// this package no longer reads or parses agents.yaml itself (that
// responsibility moved to registry once policy.yaml was eliminated and
// everything consolidated into one file) — it only defines the types and
// the small amount of behavior (AutoAllow/AutoAllowTool matching,
// Delegation/Compaction default-handling) that's independent of where the
// data came from.
//
// Deviation from the original spec's policy.yaml mockup, kept even after
// consolidating into agents.yaml: the mockup keyed auto_allow/ask on tool
// *names* like "Read" or "Bash", which are internal to each agent and not
// exposed over ACP. What ACP actually gives every agent, uniformly, is
// ToolCallUpdate.Kind (read, edit, delete, move, search, execute, think,
// fetch, switch_mode, other) — so this matches on that instead.
package policy

import (
	"strings"
	"time"

	"chorus/internal/headroom"
)

// AgentPolicy is one agent's permission settings (agents.yaml's
// auto_allow/auto_allow_tools fields, moved here from the old
// policy.yaml).
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

// Policy maps agent name -> its permission settings.
type Policy map[string]AgentPolicy

// RoutingMode selects how an unprefixed (no "<agent>: " override) prompt
// picks which agent handles it.
type RoutingMode string

const (
	// RoutingOff always sends an unprefixed prompt straight to
	// DefaultAgent — no keyword matching (removed entirely, see
	// chorus-spec.md §0) and no LLM call.
	RoutingOff RoutingMode = "off"
	// RoutingLLM asks DecisionAgent to pick both an agent and (optionally)
	// a model tier for each unprefixed prompt — see internal/router.
	RoutingLLM RoutingMode = "llm"
)

// Routing is the auto-routing config for unprefixed prompts. An explicit
// "<agent>: text" prefix always bypasses this entirely, at every mode —
// that invariant predates this type and this type doesn't change it.
type Routing struct {
	Mode string `yaml:"mode"`
	// DecisionAgent is which connected agent's session makes the
	// agent+model choice, only consulted when Mode is RoutingLLM. Should
	// be a non-metered agent (chorus warns, doesn't refuse, if it isn't —
	// see main.go's startup validation).
	DecisionAgent string `yaml:"decision_agent"`
	// ContextLevel is how much recent activity is sent to DecisionAgent
	// alongside each prompt: "prompt" (just the new prompt), "digest" (a
	// bounded rolling summary — the default), or "full" (the whole capped
	// activity log). Mutable live via the `context` REPL command — this
	// field is what that command's menu changes, not a config file value
	// re-read from disk.
	ContextLevel string `yaml:"context_level"`
	// DecisionTimeoutSeconds bounds how long chorus waits for the decision
	// agent's hidden sub-session to reply before giving up and falling
	// back to DefaultAgent. Defaults to 60s when unset or non-positive —
	// found live (chorus-spec.md §0) that a free/shared-capacity decision
	// agent's own upstream provider can be intermittently slow or need an
	// internal retry, and the original 25s default cut those off,
	// surfacing as a generic decision-failed error indistinguishable from
	// a real problem. A fast, reliable decision agent can set this lower
	// to fail faster instead.
	DecisionTimeoutSeconds int `yaml:"decision_timeout_seconds"`
	// Anonymize controls whether the text built for DecisionAgent (the
	// prompt being routed, plus ContextLevel's activity digest) has
	// obvious sensitive patterns — emails, IPv4 addresses, API-key/token-
	// shaped strings — redacted first (internal/router's anonymize.go).
	// Defaults to true when unset: the decision agent is picked for
	// routing cost/capability reasons, not necessarily one the user would
	// otherwise trust with the raw prompt, so redact-by-default is the
	// safer failure mode. Only ever applies to the hidden routing-decision
	// prompt — never to what's actually sent to the agent that handles
	// the real work.
	Anonymize *bool `yaml:"anonymize"`
}

// AnonymizeOrDefault reports whether the routing-decision prompt should be
// redacted before being sent to DecisionAgent. Defaults to true when
// unset — same opt-out (not opt-in) convention as Delegation.BriefingEnabled.
func (r Routing) AnonymizeOrDefault() bool {
	return r.Anonymize == nil || *r.Anonymize
}

// defaultDecisionTimeoutSeconds is used when DecisionTimeoutSeconds is
// unset or non-positive.
const defaultDecisionTimeoutSeconds = 60

// DecisionTimeout reports how long to wait for a routing decision before
// timing out and falling back to DefaultAgent.
func (r Routing) DecisionTimeout() time.Duration {
	if r.DecisionTimeoutSeconds <= 0 {
		return defaultDecisionTimeoutSeconds * time.Second
	}
	return time.Duration(r.DecisionTimeoutSeconds) * time.Second
}

// ModeOrDefault reports the effective RoutingMode — RoutingOff when Mode
// is empty or unrecognized, so a missing/malformed `routing:` block never
// accidentally enables a paid LLM call on every prompt.
func (r Routing) ModeOrDefault() RoutingMode {
	if RoutingMode(r.Mode) == RoutingLLM {
		return RoutingLLM
	}
	return RoutingOff
}

// ContextLevelOrDefault reports the effective context tier — "digest"
// (the cheap, mechanical default — see internal/router) when unset or
// unrecognized.
func (r Routing) ContextLevelOrDefault() string {
	switch r.ContextLevel {
	case "prompt", "full":
		return r.ContextLevel
	default:
		return "digest"
	}
}

// Delegation is optional agents.yaml config (top-level `delegation:`
// block) for whether the `delegate` MCP tool is attached at all, the
// seeded first-turn delegation briefing, and the permission-gate
// delegation nudge (CLAUDE.md's delegation-maximization plan, item 2).
type Delegation struct {
	// Enabled is the master switch — defaults to FALSE when unset. This
	// used to be implicit (delegation was always attached once 2+ agents
	// connected); made explicit and opt-in after live use showed it
	// rarely fires and the `delegate` MCP tool's schema costs ~500-1400+
	// tokens on EVERY turn of EVERY agent it's attached to, whether used
	// or not (see chorus-spec.md §0) — that cost is worth paying only when
	// a project has deliberately opted in. Mechanics (briefing/nudge) are
	// fully preserved for revisiting later, just inert while this is
	// false: main.go only attaches the delegate MCP server, wires the
	// nudge idle-checker, and seeds the briefing when EnabledOrDefault()
	// is true.
	Enabled *bool `yaml:"enabled"`
	// Briefing/Prefer/NudgeThreshold are unchanged in meaning from before
	// consolidation — see BriefingEnabled/Threshold/PreferKind. All three
	// are only ever consulted when Enabled is true.
	Briefing       *bool    `yaml:"briefing"`
	Prefer         []string `yaml:"prefer"`
	NudgeThreshold *int     `yaml:"nudge_threshold"`
}

// EnabledOrDefault reports whether delegation (the MCP tool attachment,
// briefing, and nudge) is on at all. Defaults to false when unset.
func (d Delegation) EnabledOrDefault() bool {
	return d.Enabled != nil && *d.Enabled
}

// BriefingEnabled reports whether the seeded first-turn delegation
// briefing should fire, GIVEN delegation is already enabled — callers
// must check EnabledOrDefault() first (main.go's attachDelegate already
// gates on both). Defaults to true when unset — opt-out, not opt-in,
// since the whole point of the briefing is that an agent shouldn't need
// the user to remember to ask for delegation every time once it's on.
func (d Delegation) BriefingEnabled() bool {
	return d.Briefing == nil || *d.Briefing
}

// defaultNudgeThreshold is used when nudge_threshold is unset or
// non-positive — picked as "a small handful," not derived from data.
const defaultNudgeThreshold = 3

// Threshold reports how many direct Prefer-kind tool calls trigger a
// delegation nudge. Defaults to defaultNudgeThreshold when NudgeThreshold
// is unset or <= 0 (a configured 0 or negative value would either nudge
// on every single call or never validate, neither of which is a sensible
// "unset" behavior).
func (d Delegation) Threshold() int {
	if d.NudgeThreshold == nil || *d.NudgeThreshold <= 0 {
		return defaultNudgeThreshold
	}
	return *d.NudgeThreshold
}

// PreferKind reports whether kind is one of Prefer's listed ToolKinds.
func (d Delegation) PreferKind(kind string) bool {
	if kind == "" {
		return false
	}
	for _, k := range d.Prefer {
		if k == kind {
			return true
		}
	}
	return false
}

// Compaction is optional agents.yaml config (top-level `compaction:`
// block) for proactively triggering a compaction-style command once an
// agent's context usage crosses a threshold, instead of waiting for
// (or never getting) the agent's own late/automatic compaction.
type Compaction struct {
	Enabled          *bool    `yaml:"enabled"`
	ThresholdPercent int      `yaml:"threshold_percent"`
	Aliases          []string `yaml:"aliases"`
}

// EnabledOrDefault defaults to false when unset.
func (c Compaction) EnabledOrDefault() bool {
	return c.Enabled != nil && *c.Enabled
}

// defaultCompactionThreshold: research into real-world agent usage
// patterns (chorus-spec.md §0) found compacting deliberately around 60%
// usage produces much better, cheaper summaries than waiting for an
// agent's own late auto-compaction (which can trigger multiple times per
// long session at 100K+ tokens each) — used as the default when unset or
// invalid.
const defaultCompactionThreshold = 60

// Threshold reports the context-usage percentage (0-100) that triggers
// compaction. Defaults to defaultCompactionThreshold when unset or
// outside (0,100].
func (c Compaction) Threshold() int {
	if c.ThresholdPercent <= 0 || c.ThresholdPercent > 100 {
		return defaultCompactionThreshold
	}
	return c.ThresholdPercent
}

// defaultCompactionAliases is used when Aliases is empty — matched
// case-insensitively as a substring against each agent's OWN advertised
// command names (never hardcoded as a literal "/compact" sent blind —
// see internal/tui's findCommandByAlias).
var defaultCompactionAliases = []string{"compact", "summarize", "condense"}

// AliasesOrDefault reports the effective alias list.
func (c Compaction) AliasesOrDefault() []string {
	if len(c.Aliases) == 0 {
		return defaultCompactionAliases
	}
	return c.Aliases
}

// Config is agents.yaml's fully parsed non-registry content — decoded by
// internal/registry.Parse (the single decode point for the whole file)
// alongside the []session.Spec list.
type Config struct {
	Agents     Policy
	Delegation Delegation
	Compaction Compaction
	Routing    Routing
	// Headroom is optional agents.yaml config (top-level `headroom:`
	// block) for a chorus-managed Headroom compression proxy container
	// (internal/headroom) — see that package's doc comment. Off by
	// default when unset, same opt-in convention as Delegation/Compaction.
	Headroom headroom.Config
	// DefaultAgent is used whenever routing is off, or an LLM routing
	// decision fails to resolve to a known agent+model.
	DefaultAgent string
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
