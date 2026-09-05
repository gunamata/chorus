// Package registry implements the agents.yaml registry: agent spawn
// configuration, permission settings, and delegation/compaction/routing
// config all live in this one file (data, not code) — adding an agent, or
// changing a permission/routing setting, is a config edit rather than a
// code change. AgentSession itself doesn't need to change to add an
// agent — see chorus-spec.md §10.
//
// This is the SINGLE decode point for the whole file (since the
// policy.yaml/agents.yaml split was eliminated — chorus-spec.md §0):
// Parse returns both the []session.Spec list AND the policy.Config that
// used to come from a separate policy.Parse.
package registry

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"chorus/internal/policy"
	"chorus/internal/session"
)

// entry is one agents.yaml list item.
type entry struct {
	Name string `yaml:"name"`
	// Spawn is [command, arg...]. entry[0] is the executable, the rest are
	// its arguments — mirrors §6's spawn_agent_process(cfg["spawn"]) idea.
	Spawn []string `yaml:"spawn"`
	// CostTier and Notes are surfaced to every agent via the `delegate`
	// tool's description and the seeded first-turn briefing
	// (internal/delegate, chorus-spec.md §11) — both are optional free
	// text, not validated against a fixed vocabulary. Transport must be
	// "acp" (or omitted) today — chorus only speaks ACP.
	CostTier  string `yaml:"cost_tier"`
	Notes     string `yaml:"notes"`
	Transport string `yaml:"transport"`

	// WorkDir is the in-container cwd sent to this agent over ACP when its
	// spawn command runs it inside a container (a sandboxed Claude/opencode
	// spawned via `docker run -v {{CWD}}:/workspace ...`) — see
	// session.Spec.WorkDir's doc comment. Omit for a normal, non-sandboxed
	// spawn.
	WorkDir string `yaml:"workdir"`

	// AutoAllow/AutoAllowTools moved here from the old policy.yaml — see
	// policy.AgentPolicy's doc comment for their meaning.
	AutoAllow      []string `yaml:"auto_allow"`
	AutoAllowTools []string `yaml:"auto_allow_tools"`

	// Models is this agent's optional model catalog — see
	// session.ModelInfo. Absent/empty is fine; the LLM router then never
	// picks a model for this agent, only the agent itself.
	Models []session.ModelInfo `yaml:"models"`

	// AutoMode is this agent's ACP session-mode id/name for the `auto`
	// REPL command — see session.Spec.AutoMode's doc comment. Omit until
	// you've discovered the real value via the `modes` REPL command;
	// chorus never guesses one on your behalf.
	AutoMode string `yaml:"auto_mode"`
}

// file is agents.yaml's whole top-level shape.
type file struct {
	DefaultAgent string            `yaml:"default_agent"`
	Delegation   policy.Delegation `yaml:"delegation"`
	Compaction   policy.Compaction `yaml:"compaction"`
	Routing      policy.Routing    `yaml:"routing"`
	Agents       []entry           `yaml:"agents"`
}

// Load reads agents.yaml at path. Thin wrapper around Parse — see its doc
// comment for the actual decode logic, shared with main.go's
// embedded-default fallback (compiled-in bytes, no file on disk at all).
func Load(path string) ([]session.Spec, policy.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, policy.Config{}, err
	}
	return Parse(b)
}

// Parse decodes agents.yaml content already read into memory, returning
// agent specs in file order (deliberately a list, not a map keyed by
// name — a YAML map decodes into a Go map, which has no iteration order,
// so agent startup order, and therefore the order startup messages print
// in, would vary run to run) alongside the rest of the file's config.
func Parse(b []byte) ([]session.Spec, policy.Config, error) {
	var f file
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, policy.Config{}, fmt.Errorf("agents.yaml: %w", err)
	}

	cfg := policy.Config{
		Agents:       policy.Policy{},
		Delegation:   f.Delegation,
		Compaction:   f.Compaction,
		Routing:      f.Routing,
		DefaultAgent: f.DefaultAgent,
	}

	specs := make([]session.Spec, 0, len(f.Agents))
	seen := make(map[string]bool)
	for _, e := range f.Agents {
		if e.Name == "" {
			return nil, policy.Config{}, fmt.Errorf("agents.yaml: an entry is missing 'name'")
		}
		if seen[e.Name] {
			return nil, policy.Config{}, fmt.Errorf("agents.yaml: duplicate agent name %q", e.Name)
		}
		seen[e.Name] = true
		if len(e.Spawn) == 0 {
			return nil, policy.Config{}, fmt.Errorf("agents.yaml: %s: 'spawn' must have at least one element (the command)", e.Name)
		}
		transport := e.Transport
		if transport == "" {
			transport = "acp"
		}
		if transport != "acp" {
			return nil, policy.Config{}, fmt.Errorf("agents.yaml: %s: unsupported transport %q (only \"acp\" is implemented — see chorus-spec.md §10)", e.Name, transport)
		}
		specs = append(specs, session.Spec{
			Name:     e.Name,
			Command:  e.Spawn[0],
			Args:     e.Spawn[1:],
			WorkDir:  e.WorkDir,
			CostTier: e.CostTier,
			Notes:    e.Notes,
			Models:   e.Models,
			AutoMode: e.AutoMode,
		})
		cfg.Agents[e.Name] = policy.AgentPolicy{
			AutoAllow:      e.AutoAllow,
			AutoAllowTools: e.AutoAllowTools,
		}
	}
	return specs, cfg, nil
}
