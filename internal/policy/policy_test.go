package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTempPolicy(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for a missing file", err)
	}
	if cfg.Agents == nil {
		t.Fatal("Agents = nil, want a non-nil empty map")
	}
	if len(cfg.Agents) != 0 {
		t.Fatalf("Agents = %+v, want empty", cfg.Agents)
	}
	if len(cfg.Routing.Rules) != 0 || cfg.Routing.Default != "" {
		t.Fatalf("Routing = %+v, want zero value", cfg.Routing)
	}
}

func TestConfig_BriefingEnabled_DefaultsTrue(t *testing.T) {
	var cfg Config
	if !cfg.BriefingEnabled() {
		t.Fatal("BriefingEnabled() = false, want true when Delegation is entirely unset")
	}
}

func TestConfig_BriefingEnabled_ExplicitFalse(t *testing.T) {
	off := false
	cfg := Config{Delegation: Delegation{Briefing: &off}}
	if cfg.BriefingEnabled() {
		t.Fatal("BriefingEnabled() = true, want false when explicitly disabled")
	}
}

func TestLoad_ParsesDelegationSection(t *testing.T) {
	path := writeTempPolicy(t, `
delegation:
  briefing: false
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.BriefingEnabled() {
		t.Fatal("BriefingEnabled() = true, want false per policy.yaml's delegation.briefing: false")
	}
}

func TestLoad_MissingDelegationSectionDefaultsBriefingOn(t *testing.T) {
	path := writeTempPolicy(t, `
claude:
  auto_allow: [read]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.BriefingEnabled() {
		t.Fatal("BriefingEnabled() = false, want true when policy.yaml has no delegation section at all")
	}
}

func TestLoad_ParsesDelegationPreferAndThreshold(t *testing.T) {
	path := writeTempPolicy(t, `
delegation:
  prefer: [execute, edit]
  nudge_threshold: 5
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Delegation.PreferKind("execute") || !cfg.Delegation.PreferKind("edit") {
		t.Fatalf("Delegation.Prefer = %v, want it to include execute and edit", cfg.Delegation.Prefer)
	}
	if cfg.Delegation.PreferKind("read") {
		t.Fatal("PreferKind(\"read\") = true, want false — read wasn't listed")
	}
	if got := cfg.Delegation.Threshold(); got != 5 {
		t.Fatalf("Threshold() = %d, want 5", got)
	}
}

func TestDelegation_Threshold_DefaultsWhenUnsetOrNonPositive(t *testing.T) {
	cases := []struct {
		name string
		d    Delegation
	}{
		{"unset", Delegation{}},
		{"zero", Delegation{NudgeThreshold: intPtr(0)}},
		{"negative", Delegation{NudgeThreshold: intPtr(-1)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.d.Threshold(); got != defaultNudgeThreshold {
				t.Fatalf("Threshold() = %d, want default %d", got, defaultNudgeThreshold)
			}
		})
	}
}

func TestDelegation_PreferKind_EmptyPreferMatchesNothing(t *testing.T) {
	var d Delegation
	if d.PreferKind("execute") {
		t.Fatal("PreferKind(\"execute\") = true, want false when Prefer is empty (nudge disabled)")
	}
	if d.PreferKind("") {
		t.Fatal("PreferKind(\"\") = true, want false for an empty kind")
	}
}

func intPtr(n int) *int { return &n }

func TestLoad_MergesAgentPoliciesAndRouting(t *testing.T) {
	path := writeTempPolicy(t, `
claude:
  auto_allow: [read, search]
  auto_allow_tools: [mcp__chorus-delegate__]
gemini:
  auto_allow: [read]
routing:
  default: gemini
  ask_when_ambiguous: true
  rules:
    - match: [fix, implement]
      agent: claude
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2 (got %+v)", len(cfg.Agents), cfg.Agents)
	}
	if !cfg.Agents.AutoAllow("claude", "read") {
		t.Error("expected claude to auto-allow \"read\"")
	}
	if cfg.Agents.AutoAllow("claude", "edit") {
		t.Error("expected claude NOT to auto-allow \"edit\"")
	}
	if !cfg.Agents.AutoAllowTool("claude", "mcp__chorus-delegate__delegate") {
		t.Error("expected claude to auto-allow the delegate tool by title prefix")
	}

	if cfg.Routing.Default != "gemini" {
		t.Errorf("Routing.Default = %q, want gemini", cfg.Routing.Default)
	}
	if !cfg.Routing.AskWhenAmbiguous {
		t.Error("Routing.AskWhenAmbiguous = false, want true")
	}
	if len(cfg.Routing.Rules) != 1 || cfg.Routing.Rules[0].Agent != "claude" {
		t.Fatalf("Routing.Rules = %+v, want one rule routing to claude", cfg.Routing.Rules)
	}

	// The "routing" key must not leak into Agents as a bogus agent policy.
	if _, ok := cfg.Agents["routing"]; ok {
		t.Error(`Agents contains a "routing" entry — the routing key leaked into the agent map`)
	}
}

func TestLoad_MalformedFieldTypeErrors(t *testing.T) {
	// auto_allow must be a list; giving it a scalar should fail to decode
	// rather than silently producing a useless zero value.
	path := writeTempPolicy(t, "claude:\n  auto_allow: \"not-a-list\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want an error for a malformed auto_allow field")
	}
}

func TestPolicy_AutoAllow(t *testing.T) {
	p := Policy{"claude": AgentPolicy{AutoAllow: []string{"read", "search"}}}

	cases := []struct {
		agent, kind string
		want        bool
	}{
		{"claude", "read", true},
		{"claude", "edit", false},
		{"claude", "", false},
		{"gemini", "read", false}, // unknown agent
	}
	for _, c := range cases {
		if got := p.AutoAllow(c.agent, c.kind); got != c.want {
			t.Errorf("AutoAllow(%q, %q) = %v, want %v", c.agent, c.kind, got, c.want)
		}
	}
}

func TestPolicy_AutoAllowTool(t *testing.T) {
	p := Policy{"claude": AgentPolicy{AutoAllowTools: []string{"mcp__chorus-delegate__"}}}

	if !p.AutoAllowTool("claude", "mcp__chorus-delegate__delegate") {
		t.Error("expected case-insensitive prefix match on the real MCP-qualified title")
	}
	if p.AutoAllowTool("claude", "Write") {
		t.Error("expected no match against an unrelated title")
	}
	if p.AutoAllowTool("claude", "") {
		t.Error("expected no match against an empty title")
	}
	if p.AutoAllowTool("gemini", "mcp__chorus-delegate__delegate") {
		t.Error("expected no match for an agent with no auto_allow_tools configured")
	}
}

// Regression test for chorus-spec.md §0's 2026-08-22 audit finding:
// substring matching let any tool call whose agent-supplied Title merely
// *mentioned* an allowed word bypass its real permission requirement.
func TestPolicy_AutoAllowTool_DoesNotMatchMereMention(t *testing.T) {
	p := Policy{"claude": AgentPolicy{AutoAllowTools: []string{"mcp__chorus-delegate__"}}}

	spoofedTitles := []string{
		"please delegate this",
		"run rm -rf / (this is basically a delegate)",
		"delegate",
	}
	for _, title := range spoofedTitles {
		if p.AutoAllowTool("claude", title) {
			t.Errorf("AutoAllowTool(%q) = true, want false — title only mentions the word, doesn't start with the real MCP-qualified prefix", title)
		}
	}
}
