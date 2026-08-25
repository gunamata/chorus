// Package registry implements the §10 agents.yaml registry: agent spawn
// configuration lives in data, not code, so adding an agent is a registry
// entry rather than a code change. AgentSession itself doesn't need to
// change to add one — see chorus-spec.md §10.
package registry

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

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
}

type file struct {
	Agents []entry `yaml:"agents"`
}

// Load reads agents.yaml at path and returns agent specs in file order.
// Thin wrapper around Parse — see its doc comment for the actual decode
// logic, shared with main.go's embedded-default fallback (compiled-in
// bytes, no file on disk at all).
func Load(path string) ([]session.Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes agents.yaml content already read into memory, in file
// order.
//
// Deviation from the spec's agents.yaml mockup, noted deliberately (§0's
// priority order): the mockup showed a top-level map keyed by agent name
// (claude: ..., gemini: ...). A YAML map decodes into a Go map, which has
// no iteration order — agent startup order (and therefore the order
// startup messages print in) would vary run to run. Using a top-level
// `agents:` list instead preserves file order deterministically.
func Parse(b []byte) ([]session.Spec, error) {
	var f file
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("agents.yaml: %w", err)
	}

	specs := make([]session.Spec, 0, len(f.Agents))
	seen := make(map[string]bool)
	for _, e := range f.Agents {
		if e.Name == "" {
			return nil, fmt.Errorf("agents.yaml: an entry is missing 'name'")
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("agents.yaml: duplicate agent name %q", e.Name)
		}
		seen[e.Name] = true
		if len(e.Spawn) == 0 {
			return nil, fmt.Errorf("agents.yaml: %s: 'spawn' must have at least one element (the command)", e.Name)
		}
		transport := e.Transport
		if transport == "" {
			transport = "acp"
		}
		if transport != "acp" {
			return nil, fmt.Errorf("agents.yaml: %s: unsupported transport %q (only \"acp\" is implemented — see chorus-spec.md §10)", e.Name, transport)
		}
		specs = append(specs, session.Spec{
			Name:     e.Name,
			Command:  e.Spawn[0],
			Args:     e.Spawn[1:],
			CostTier: e.CostTier,
			Notes:    e.Notes,
		})
	}
	return specs, nil
}
