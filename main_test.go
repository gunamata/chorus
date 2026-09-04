package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chorus/internal/delegate"
	"chorus/internal/policy"
	"chorus/internal/session"
)

func TestBuildBriefingText_MentionsEachRosterAgent(t *testing.T) {
	roster := []delegate.RosterEntry{
		{Name: "opencode", CostTier: "free", Notes: "catch-all agent"},
		{Name: "gemini", CostTier: "seat", Notes: "summarize/explain"},
	}
	text := buildBriefingText(roster)
	for _, want := range []string{"opencode", "free", "catch-all agent", "gemini", "seat", "summarize/explain"} {
		if !strings.Contains(text, want) {
			t.Errorf("buildBriefingText() = %q, want it to contain %q", text, want)
		}
	}
	if !strings.Contains(text, "delegate") {
		t.Errorf("buildBriefingText() = %q, want it to mention the delegate tool", text)
	}
	if !strings.Contains(strings.ToLower(text), "unverified") {
		t.Errorf("buildBriefingText() = %q, want verification framing present", text)
	}
}

func TestBuildBriefingText_EmptyRoster(t *testing.T) {
	text := buildBriefingText(nil)
	if text == "" {
		t.Fatal("buildBriefingText(nil) = \"\", want non-empty text even with no roster entries")
	}
}

// --- loadAgentConfig: embedded defaults, local-file precedence ----------
//
// policy.yaml no longer exists (chorus-spec.md §0) — everything lives in
// one agents.yaml, so there's no more "local agents requires local
// policy" guard to test; a local agents.yaml is simply all-or-nothing.

const testAgentsYAML = `
default_agent: test-only-agent
agents:
  - name: test-only-agent
    spawn: ["true"]
    cost_tier: free
`

func TestLoadAgentConfig_FallsBackToEmbeddedDefaultsWhenNoLocalFile(t *testing.T) {
	// An empty CHORUS_HOME guarantees no central agents.yaml exists either
	// — without this, the test would silently pass or fail depending on
	// whether the machine running it happens to have a seeded
	// ~/.chorus/agents.yaml (e.g. from install.sh), which is exactly the
	// kind of environment-dependent flakiness a unit test must not have.
	t.Setenv("CHORUS_HOME", t.TempDir())
	agentSpecs, cfg, err := loadAgentConfig(t.TempDir(), "")
	if err != nil {
		t.Fatalf("loadAgentConfig() error = %v, want it to fall back to the embedded default cleanly", err)
	}
	if len(agentSpecs) == 0 {
		t.Fatal("agentSpecs is empty, want the embedded default agent set")
	}
	var names []string
	for _, s := range agentSpecs {
		names = append(names, s.Name)
	}
	if !contains(names, "claude") {
		t.Fatalf("agentSpecs = %v, want the embedded default's \"claude\" entry present", names)
	}
	if cfg.DefaultAgent == "" {
		t.Fatal("cfg.DefaultAgent is empty, want the embedded default's default_agent")
	}
}

func TestLoadAgentConfig_LocalFileTakesPrecedenceOverEmbedded(t *testing.T) {
	t.Setenv("CHORUS_HOME", t.TempDir())
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agents.yaml"), testAgentsYAML)

	agentSpecs, cfg, err := loadAgentConfig(dir, "")
	if err != nil {
		t.Fatalf("loadAgentConfig() error = %v", err)
	}
	if len(agentSpecs) != 1 || agentSpecs[0].Name != "test-only-agent" {
		t.Fatalf("agentSpecs = %+v, want only the local file's test-only-agent (local must win over embedded)", agentSpecs)
	}
	if cfg.DefaultAgent != "test-only-agent" {
		t.Fatalf("cfg.DefaultAgent = %q, want the local file's value", cfg.DefaultAgent)
	}
}

// --- loadAgentConfig: central ~/.chorus/agents.yaml (2026-09-03) --------

func TestLoadAgentConfig_CentralFileUsedWhenNoLocalFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHORUS_HOME", home)
	writeFile(t, filepath.Join(home, "agents.yaml"), testAgentsYAML)

	agentSpecs, cfg, err := loadAgentConfig(t.TempDir(), "")
	if err != nil {
		t.Fatalf("loadAgentConfig() error = %v", err)
	}
	if len(agentSpecs) != 1 || agentSpecs[0].Name != "test-only-agent" {
		t.Fatalf("agentSpecs = %+v, want the central file's test-only-agent used when no local agents.yaml exists", agentSpecs)
	}
	if cfg.DefaultAgent != "test-only-agent" {
		t.Fatalf("cfg.DefaultAgent = %q, want the central file's value", cfg.DefaultAgent)
	}
}

func TestLoadAgentConfig_LocalFileTakesPrecedenceOverCentral(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHORUS_HOME", home)
	writeFile(t, filepath.Join(home, "agents.yaml"), `
default_agent: central-agent
agents:
  - name: central-agent
    spawn: ["true"]
    cost_tier: free
`)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agents.yaml"), testAgentsYAML)

	agentSpecs, cfg, err := loadAgentConfig(dir, "")
	if err != nil {
		t.Fatalf("loadAgentConfig() error = %v", err)
	}
	if len(agentSpecs) != 1 || agentSpecs[0].Name != "test-only-agent" {
		t.Fatalf("agentSpecs = %+v, want the local file's test-only-agent (local must win over central)", agentSpecs)
	}
	if cfg.DefaultAgent != "test-only-agent" {
		t.Fatalf("cfg.DefaultAgent = %q, want the local file's value, not the central one", cfg.DefaultAgent)
	}
}

func TestLoadAgentConfig_FallsBackToEmbeddedWhenCentralFileAbsent(t *testing.T) {
	// A CHORUS_HOME that exists but has no agents.yaml in it yet (the
	// state before install.sh/install.ps1 ever seed one, or right after
	// CHORUS_HOME is pointed somewhere new) must still fall through
	// cleanly to the embedded default, not error.
	t.Setenv("CHORUS_HOME", t.TempDir())
	agentSpecs, _, err := loadAgentConfig(t.TempDir(), "")
	if err != nil {
		t.Fatalf("loadAgentConfig() error = %v, want a clean fallback to embedded when CHORUS_HOME has no agents.yaml", err)
	}
	if !contains(specNames(agentSpecs), "claude") {
		t.Fatalf("agentSpecs = %v, want the embedded default's \"claude\" entry present", specNames(agentSpecs))
	}
}

func specNames(specs []session.Spec) []string {
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return names
}

// --- loadAgentConfig: --agents=<path> override ---------------------------
//
// Added so more than one config can coexist in the same directory without
// renaming (e.g. agents.yaml.sandbox alongside the default agents.yaml),
// selected explicitly per invocation via --agents=<path>.

func TestLoadAgentConfig_OverrideTakesPrecedenceOverLocalAndEmbedded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agents.yaml"), testAgentsYAML)
	writeFile(t, filepath.Join(dir, "agents.yaml.sandbox"), `
default_agent: sandboxed-agent
agents:
  - name: sandboxed-agent
    spawn: ["true"]
    cost_tier: free
`)

	agentSpecs, cfg, err := loadAgentConfig(dir, "agents.yaml.sandbox")
	if err != nil {
		t.Fatalf("loadAgentConfig() error = %v", err)
	}
	if len(agentSpecs) != 1 || agentSpecs[0].Name != "sandboxed-agent" {
		t.Fatalf("agentSpecs = %+v, want only the override file's sandboxed-agent (override must win over local agents.yaml)", agentSpecs)
	}
	if cfg.DefaultAgent != "sandboxed-agent" {
		t.Fatalf("cfg.DefaultAgent = %q, want the override file's value", cfg.DefaultAgent)
	}
}

func TestLoadAgentConfig_OverrideResolvesRelativeToCwd(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agents.yaml.sandbox"), testAgentsYAML)

	agentSpecs, _, err := loadAgentConfig(dir, "agents.yaml.sandbox")
	if err != nil {
		t.Fatalf("loadAgentConfig() error = %v", err)
	}
	if len(agentSpecs) != 1 || agentSpecs[0].Name != "test-only-agent" {
		t.Fatalf("agentSpecs = %+v, want the relative override file resolved against cwd", agentSpecs)
	}
}

func TestLoadAgentConfig_MissingOverrideIsAHardError(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := loadAgentConfig(dir, "does-not-exist.yaml"); err == nil {
		t.Fatal("loadAgentConfig() error = nil, want an error for a missing --agents override (unlike the no-override case, this must not silently fall back to the embedded default)")
	}
}

func TestFlagValue(t *testing.T) {
	if got := flagValue([]string{"--fresh", "--agents=agents.yaml.sandbox"}, "--agents="); got != "agents.yaml.sandbox" {
		t.Errorf("flagValue() = %q, want %q", got, "agents.yaml.sandbox")
	}
	if got := flagValue([]string{"--fresh"}, "--agents="); got != "" {
		t.Errorf("flagValue() = %q, want empty when the flag is absent", got)
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "present.txt")
	if ok, err := fileExists(path); err != nil || ok {
		t.Fatalf("fileExists(%q) = (%v, %v), want (false, nil) before the file is created", path, ok, err)
	}
	writeFile(t, path, "x")
	if ok, err := fileExists(path); err != nil || !ok {
		t.Fatalf("fileExists(%q) = (%v, %v), want (true, nil) once the file exists", path, ok, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write test fixture %s: %v", path, err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// --- validateRoutingConfig ------------------------------------------

func TestValidateRoutingConfig_OffModeSkipsValidation(t *testing.T) {
	err := validateRoutingConfig(nil, policy.Routing{Mode: "off", DecisionAgent: "does-not-exist"})
	if err != nil {
		t.Fatalf("validateRoutingConfig() error = %v, want nil — decision_agent is never consulted when routing is off", err)
	}
}

func TestValidateRoutingConfig_LLMModeRequiresKnownDecisionAgent(t *testing.T) {
	specs := []session.Spec{{Name: "opencode", CostTier: "free"}}
	err := validateRoutingConfig(specs, policy.Routing{Mode: "llm", DecisionAgent: "nonexistent"})
	if err == nil {
		t.Fatal("validateRoutingConfig() error = nil, want an error for an unknown decision_agent")
	}
}

func TestValidateRoutingConfig_LLMModeAcceptsKnownNonMeteredDecisionAgent(t *testing.T) {
	specs := []session.Spec{{Name: "opencode", CostTier: "free"}}
	err := validateRoutingConfig(specs, policy.Routing{Mode: "llm", DecisionAgent: "opencode"})
	if err != nil {
		t.Fatalf("validateRoutingConfig() error = %v, want nil for a known, non-metered decision_agent", err)
	}
}

func TestValidateRoutingConfig_LLMModeAcceptsMeteredDecisionAgentWithoutErroring(t *testing.T) {
	// A metered decision_agent is a bad idea (warned about via stderr, not
	// tested here), but not an error — it's still a valid configuration.
	specs := []session.Spec{{Name: "claude", CostTier: "metered"}}
	err := validateRoutingConfig(specs, policy.Routing{Mode: "llm", DecisionAgent: "claude"})
	if err != nil {
		t.Fatalf("validateRoutingConfig() error = %v, want nil (a warning, not a hard error) for a metered decision_agent", err)
	}
}
