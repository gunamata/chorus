package delegate

import (
	"encoding/json"
	"fmt"
	"strings"

	"chorus/internal/session"
)

// RosterEntry describes one other agent available for delegation. It's the
// same data presented two ways — the delegate tool's Description
// (RunMCPServer, via EnvRoster) and the seeded first-turn briefing
// (main.go) — built from one BuildRoster call so the two surfaces can't
// drift out of sync with each other.
type RosterEntry struct {
	Name     string `json:"name"`
	CostTier string `json:"cost_tier,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

// BuildRoster returns every agent in specs that's actually connected
// (present in conns) except exclude, in specs' original order — the same
// determinism rationale as registry.Load itself (chorus-spec.md §10):
// iteration order must not vary run to run.
func BuildRoster(specs []session.Spec, conns map[string]*session.Connection, exclude string) []RosterEntry {
	var out []RosterEntry
	for _, s := range specs {
		if s.Name == exclude {
			continue
		}
		if _, ok := conns[s.Name]; !ok {
			continue
		}
		out = append(out, RosterEntry{Name: s.Name, CostTier: s.CostTier, Notes: s.Notes})
	}
	return out
}

// EncodeRoster/DecodeRoster round-trip a roster through EnvRoster's string
// value. An empty string decodes to a nil roster — indistinguishable from
// "no roster data at all" (missing env var), which is the correct behavior:
// both cases mean the caller should fall back to a generic description.
func EncodeRoster(r []RosterEntry) (string, error) {
	if len(r) == 0 {
		return "", nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func DecodeRoster(s string) ([]RosterEntry, error) {
	if s == "" {
		return nil, nil
	}
	var r []RosterEntry
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, fmt.Errorf("decode roster: %w", err)
	}
	return r, nil
}

// FormatRosterLines renders one "- name (cost tier: X): notes" line per
// entry, shared by the delegate tool's Description and the seeded
// briefing text. Entries with no cost tier or notes still print by name.
func FormatRosterLines(roster []RosterEntry) string {
	var b strings.Builder
	for _, r := range roster {
		fmt.Fprintf(&b, "- %s", r.Name)
		if r.CostTier != "" {
			fmt.Fprintf(&b, " (cost tier: %s)", r.CostTier)
		}
		if r.Notes != "" {
			fmt.Fprintf(&b, ": %s", r.Notes)
		}
		b.WriteString("\n")
	}
	return b.String()
}
