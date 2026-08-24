package registry

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTempAgents(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_PreservesFileOrder(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - name: claude
    spawn: ["npx", "-y", "@agentclientprotocol/claude-agent-acp"]
    transport: acp
  - name: gemini
    spawn: ["gemini", "--acp"]
    transport: acp
  - name: opencode
    spawn: ["opencode", "acp"]
    transport: acp
`)
	specs, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(specs) != 3 {
		t.Fatalf("len(specs) = %d, want 3", len(specs))
	}
	wantOrder := []string{"claude", "gemini", "opencode"}
	for i, want := range wantOrder {
		if specs[i].Name != want {
			t.Errorf("specs[%d].Name = %q, want %q (order not preserved)", i, specs[i].Name, want)
		}
	}
	if specs[0].Command != "npx" || len(specs[0].Args) != 2 {
		t.Errorf("specs[0] = %+v, command/args split incorrectly", specs[0])
	}
}

func TestLoad_ParsesCostTierAndNotes(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - name: claude
    spawn: ["claude-agent-acp"]
    cost_tier: metered
    notes: "general-purpose, metered usage"
`)
	specs, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if specs[0].CostTier != "metered" {
		t.Errorf("specs[0].CostTier = %q, want %q", specs[0].CostTier, "metered")
	}
	if specs[0].Notes != "general-purpose, metered usage" {
		t.Errorf("specs[0].Notes = %q, want %q", specs[0].Notes, "general-purpose, metered usage")
	}
}

func TestLoad_CostTierAndNotesOptional(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - name: claude
    spawn: ["claude-agent-acp"]
`)
	specs, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if specs[0].CostTier != "" || specs[0].Notes != "" {
		t.Errorf("specs[0] = %+v, want empty CostTier/Notes when omitted", specs[0])
	}
}

func TestLoad_TransportDefaultsToAcp(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - name: claude
    spawn: ["claude-agent-acp"]
`)
	specs, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("len(specs) = %d, want 1", len(specs))
	}
}

func TestLoad_RejectsNonAcpTransport(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - name: weird
    spawn: ["weird-agent"]
    transport: http
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want an error for an unsupported transport")
	}
}

func TestLoad_RejectsMissingName(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - spawn: ["some-agent"]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want an error for a missing name")
	}
}

func TestLoad_RejectsEmptySpawn(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - name: claude
    spawn: []
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want an error for an empty spawn list")
	}
}

func TestLoad_RejectsDuplicateNames(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - name: claude
    spawn: ["claude-agent-acp"]
  - name: claude
    spawn: ["claude-agent-acp", "--other"]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want an error for a duplicate agent name")
	}
}

func TestLoad_MissingFileErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("Load() error = nil, want an error for a missing agents.yaml (unlike policy.yaml, this file is required)")
	}
}
