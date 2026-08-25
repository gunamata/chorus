package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chorus/internal/delegate"
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

// --- loadAgentConfig: embedded defaults, local-file precedence, and the
// local-agents-requires-local-policy guard ------------------------------

const testAgentsYAML = `
agents:
  - name: test-only-agent
    spawn: ["true"]
    cost_tier: free
`

const testPolicyYAML = `
routing:
  default: test-only-agent
`

func TestLoadAgentConfig_FallsBackToEmbeddedDefaultsWhenNoLocalFiles(t *testing.T) {
	agentSpecs, cfg, err := loadAgentConfig(t.TempDir())
	if err != nil {
		t.Fatalf("loadAgentConfig() error = %v, want it to fall back to the embedded defaults cleanly", err)
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
	if cfg.Routing.Default == "" {
		t.Fatal("cfg.Routing.Default is empty, want the embedded default policy's routing.default")
	}
}

func TestLoadAgentConfig_LocalAgentsWithoutLocalPolicyErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agents.yaml"), testAgentsYAML)

	_, _, err := loadAgentConfig(dir)
	if err == nil {
		t.Fatal("loadAgentConfig() error = nil, want an error: a local agents.yaml with no local policy.yaml must be rejected")
	}
	if !strings.Contains(err.Error(), "policy.yaml") {
		t.Fatalf("loadAgentConfig() error = %q, want it to mention policy.yaml", err)
	}
}

func TestLoadAgentConfig_LocalFilesTakePrecedenceOverEmbedded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agents.yaml"), testAgentsYAML)
	writeFile(t, filepath.Join(dir, "policy.yaml"), testPolicyYAML)

	agentSpecs, cfg, err := loadAgentConfig(dir)
	if err != nil {
		t.Fatalf("loadAgentConfig() error = %v", err)
	}
	if len(agentSpecs) != 1 || agentSpecs[0].Name != "test-only-agent" {
		t.Fatalf("agentSpecs = %+v, want only the local file's test-only-agent (local must win over embedded)", agentSpecs)
	}
	if cfg.Routing.Default != "test-only-agent" {
		t.Fatalf("cfg.Routing.Default = %q, want the local policy.yaml's value", cfg.Routing.Default)
	}
}

func TestLoadAgentConfig_LocalPolicyAloneAllowedWithEmbeddedAgents(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "policy.yaml"), testPolicyYAML)

	agentSpecs, cfg, err := loadAgentConfig(dir)
	if err != nil {
		t.Fatalf("loadAgentConfig() error = %v, want a local policy.yaml with no local agents.yaml to be allowed", err)
	}
	var names []string
	for _, s := range agentSpecs {
		names = append(names, s.Name)
	}
	if !contains(names, "claude") {
		t.Fatalf("agentSpecs = %v, want the embedded default agent set since no local agents.yaml was provided", names)
	}
	if cfg.Routing.Default != "test-only-agent" {
		t.Fatalf("cfg.Routing.Default = %q, want the local policy.yaml's value to still apply", cfg.Routing.Default)
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
