package policy

import (
	"testing"
	"time"
)

func intPtr(n int) *int    { return &n }
func boolPtr(b bool) *bool { return &b }

// --- Delegation ----------------------------------------------------

func TestDelegation_EnabledOrDefault_DefaultsFalse(t *testing.T) {
	var d Delegation
	if d.EnabledOrDefault() {
		t.Fatal("EnabledOrDefault() = true, want false when Enabled is unset")
	}
	d.Enabled = boolPtr(true)
	if !d.EnabledOrDefault() {
		t.Fatal("EnabledOrDefault() = false, want true when explicitly enabled")
	}
	d.Enabled = boolPtr(false)
	if d.EnabledOrDefault() {
		t.Fatal("EnabledOrDefault() = true, want false when explicitly disabled")
	}
}

func TestDelegation_BriefingEnabled_DefaultsTrue(t *testing.T) {
	var d Delegation
	if !d.BriefingEnabled() {
		t.Fatal("BriefingEnabled() = false, want true when Briefing is unset")
	}
	d.Briefing = boolPtr(false)
	if d.BriefingEnabled() {
		t.Fatal("BriefingEnabled() = true, want false when explicitly disabled")
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
	if got := (Delegation{NudgeThreshold: intPtr(5)}).Threshold(); got != 5 {
		t.Fatalf("Threshold() = %d, want 5", got)
	}
}

func TestDelegation_PreferKind(t *testing.T) {
	var empty Delegation
	if empty.PreferKind("execute") {
		t.Fatal("PreferKind(\"execute\") = true, want false when Prefer is empty (nudge disabled)")
	}
	if empty.PreferKind("") {
		t.Fatal("PreferKind(\"\") = true, want false for an empty kind")
	}

	d := Delegation{Prefer: []string{"execute", "edit"}}
	if !d.PreferKind("execute") || !d.PreferKind("edit") {
		t.Fatalf("Prefer = %v, want it to include execute and edit", d.Prefer)
	}
	if d.PreferKind("read") {
		t.Fatal("PreferKind(\"read\") = true, want false — read wasn't listed")
	}
}

// --- Compaction ------------------------------------------------------

func TestCompaction_EnabledOrDefault_DefaultsFalse(t *testing.T) {
	var c Compaction
	if c.EnabledOrDefault() {
		t.Fatal("EnabledOrDefault() = true, want false when unset")
	}
	c.Enabled = boolPtr(true)
	if !c.EnabledOrDefault() {
		t.Fatal("EnabledOrDefault() = false, want true when explicitly enabled")
	}
}

func TestCompaction_Threshold_DefaultsWhenUnsetOrInvalid(t *testing.T) {
	cases := []Compaction{{}, {ThresholdPercent: 0}, {ThresholdPercent: -5}, {ThresholdPercent: 150}}
	for _, c := range cases {
		if got := c.Threshold(); got != defaultCompactionThreshold {
			t.Fatalf("Threshold() = %d, want default %d for %+v", got, defaultCompactionThreshold, c)
		}
	}
	if got := (Compaction{ThresholdPercent: 75}).Threshold(); got != 75 {
		t.Fatalf("Threshold() = %d, want 75", got)
	}
}

func TestCompaction_AliasesOrDefault(t *testing.T) {
	var c Compaction
	if got := c.AliasesOrDefault(); len(got) == 0 {
		t.Fatal("AliasesOrDefault() = empty, want the built-in default aliases")
	}
	custom := Compaction{Aliases: []string{"squash"}}
	if got := custom.AliasesOrDefault(); len(got) != 1 || got[0] != "squash" {
		t.Fatalf("AliasesOrDefault() = %v, want the configured [squash]", got)
	}
}

// --- Routing ---------------------------------------------------------

func TestRouting_ModeOrDefault(t *testing.T) {
	cases := []struct {
		mode string
		want RoutingMode
	}{
		{"", RoutingOff},
		{"garbage", RoutingOff},
		{"off", RoutingOff},
		{"llm", RoutingLLM},
	}
	for _, c := range cases {
		if got := (Routing{Mode: c.mode}).ModeOrDefault(); got != c.want {
			t.Errorf("Routing{Mode: %q}.ModeOrDefault() = %v, want %v", c.mode, got, c.want)
		}
	}
}

func TestRouting_ContextLevelOrDefault(t *testing.T) {
	cases := []struct {
		level string
		want  string
	}{
		{"", "digest"},
		{"garbage", "digest"},
		{"prompt", "prompt"},
		{"digest", "digest"},
		{"full", "full"},
	}
	for _, c := range cases {
		if got := (Routing{ContextLevel: c.level}).ContextLevelOrDefault(); got != c.want {
			t.Errorf("Routing{ContextLevel: %q}.ContextLevelOrDefault() = %q, want %q", c.level, got, c.want)
		}
	}
}

func TestRouting_DecisionTimeout_DefaultsWhenUnsetOrNonPositive(t *testing.T) {
	cases := []int{0, -1, -100}
	for _, secs := range cases {
		got := (Routing{DecisionTimeoutSeconds: secs}).DecisionTimeout()
		if got != defaultDecisionTimeoutSeconds*time.Second {
			t.Errorf("Routing{DecisionTimeoutSeconds: %d}.DecisionTimeout() = %v, want the %ds default", secs, got, defaultDecisionTimeoutSeconds)
		}
	}
	if got := (Routing{DecisionTimeoutSeconds: 90}).DecisionTimeout(); got != 90*time.Second {
		t.Errorf("DecisionTimeout() = %v, want 90s", got)
	}
}

func TestRouting_AnonymizeOrDefault_DefaultsTrue(t *testing.T) {
	var r Routing
	if !r.AnonymizeOrDefault() {
		t.Fatal("AnonymizeOrDefault() = false, want true when Anonymize is unset")
	}
	r.Anonymize = boolPtr(false)
	if r.AnonymizeOrDefault() {
		t.Fatal("AnonymizeOrDefault() = true, want false when explicitly disabled")
	}
	r.Anonymize = boolPtr(true)
	if !r.AnonymizeOrDefault() {
		t.Fatal("AnonymizeOrDefault() = false, want true when explicitly enabled")
	}
}

// --- Policy (auto_allow / auto_allow_tools) ---------------------------

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
