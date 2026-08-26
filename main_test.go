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
	agentSpecs, cfg, err := loadAgentConfig(t.TempDir())
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
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agents.yaml"), testAgentsYAML)

	agentSpecs, cfg, err := loadAgentConfig(dir)
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
