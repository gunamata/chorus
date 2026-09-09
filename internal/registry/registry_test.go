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
	specs, _, err := Load(path)
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
	specs, _, err := Load(path)
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
	specs, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if specs[0].CostTier != "" || specs[0].Notes != "" {
		t.Errorf("specs[0] = %+v, want empty CostTier/Notes when omitted", specs[0])
	}
}

func TestLoad_ParsesAutoMode(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - name: claude
    spawn: ["claude-agent-acp"]
    auto_mode: acceptEdits
`)
	specs, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if specs[0].AutoMode != "acceptEdits" {
		t.Errorf("specs[0].AutoMode = %q, want %q", specs[0].AutoMode, "acceptEdits")
	}
}

func TestLoad_AutoModeOptional(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - name: claude
    spawn: ["claude-agent-acp"]
`)
	specs, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if specs[0].AutoMode != "" {
		t.Errorf("specs[0].AutoMode = %q, want empty when omitted", specs[0].AutoMode)
	}
}

func TestLoad_TransportDefaultsToAcp(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - name: claude
    spawn: ["claude-agent-acp"]
`)
	specs, _, err := Load(path)
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
	if _, _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want an error for an unsupported transport")
	}
}

func TestLoad_RejectsMissingName(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - spawn: ["some-agent"]
`)
	if _, _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want an error for a missing name")
	}
}

func TestLoad_RejectsEmptySpawn(t *testing.T) {
	path := writeTempAgents(t, `
agents:
  - name: claude
    spawn: []
`)
	if _, _, err := Load(path); err == nil {
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
	if _, _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want an error for a duplicate agent name")
	}
}

func TestLoad_MissingFileErrors(t *testing.T) {
	if _, _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("Load() error = nil, want an error for a missing agents.yaml (chorus falls back to its embedded default at the main.go layer, not here — this package's own Load has no such fallback)")
	}
}

// --- fields moved in from the old policy.yaml, plus new top-level config
// (chorus-spec.md §0: policy.yaml eliminated, everything consolidated
// into this one file) --------------------------------------------------

func TestParse_DecodesPerAgentPermissions(t *testing.T) {
	_, cfg, err := Parse([]byte(`
agents:
  - name: claude
    spawn: ["claude-agent-acp"]
    auto_allow: [read, search, think]
    auto_allow_tools: [mcp__chorus-delegate__]
  - name: opencode
    spawn: ["opencode", "acp"]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
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
	if cfg.Agents.AutoAllow("opencode", "read") {
		t.Error("expected opencode to have no auto_allow entries configured")
	}
}

func TestParse_DecodesDefaultAgentDelegationCompactionRouting(t *testing.T) {
	_, cfg, err := Parse([]byte(`
default_agent: opencode
delegation:
  enabled: true
  prefer: [execute, edit]
  nudge_threshold: 2
compaction:
  enabled: true
  threshold_percent: 60
routing:
  mode: llm
  decision_agent: opencode
  context_level: digest
agents:
  - name: opencode
    spawn: ["opencode", "acp"]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.DefaultAgent != "opencode" {
		t.Errorf("DefaultAgent = %q, want opencode", cfg.DefaultAgent)
	}
	if !cfg.Delegation.EnabledOrDefault() {
		t.Error("Delegation.EnabledOrDefault() = false, want true")
	}
	if !cfg.Delegation.PreferKind("execute") {
		t.Error("expected Delegation.Prefer to include execute")
	}
	if !cfg.Compaction.EnabledOrDefault() || cfg.Compaction.Threshold() != 60 {
		t.Errorf("Compaction = %+v, want enabled at 60%%", cfg.Compaction)
	}
	if string(cfg.Routing.ModeOrDefault()) != "llm" {
		t.Errorf("Routing.Mode = %q, want llm", cfg.Routing.Mode)
	}
	if cfg.Routing.DecisionAgent != "opencode" {
		t.Errorf("Routing.DecisionAgent = %q, want opencode", cfg.Routing.DecisionAgent)
	}
}

func TestParse_DecodesPerAgentModels(t *testing.T) {
	_, cfg, err := Parse([]byte(`
agents:
  - name: claude
    spawn: ["claude-agent-acp"]
    models:
      - id: claude-opus-4-8
        label: Opus
        capabilities: "highest reasoning quality"
        when_to_use: "hard problems"
      - id: claude-haiku-4-5-20251001
        label: Haiku
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	_ = cfg
	specs, _, err := Parse([]byte(`
agents:
  - name: claude
    spawn: ["claude-agent-acp"]
    models:
      - id: claude-opus-4-8
        label: Opus
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(specs[0].Models) != 1 || specs[0].Models[0].ID != "claude-opus-4-8" {
		t.Fatalf("specs[0].Models = %+v, want one entry claude-opus-4-8", specs[0].Models)
	}
}

func TestParse_DecodesWorkDir(t *testing.T) {
	specs, _, err := Parse([]byte(`
agents:
  - name: opencode
    spawn: ["docker", "run", "--rm", "-i", "-v", "{{CWD}}:/workspace", "chorus-opencode-sandbox"]
    workdir: /workspace
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if specs[0].WorkDir != "/workspace" {
		t.Errorf("specs[0].WorkDir = %q, want /workspace", specs[0].WorkDir)
	}
}

func TestParse_WorkDirOptional(t *testing.T) {
	specs, _, err := Parse([]byte(`
agents:
  - name: opencode
    spawn: ["opencode", "acp"]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if specs[0].WorkDir != "" {
		t.Errorf("specs[0].WorkDir = %q, want empty when omitted", specs[0].WorkDir)
	}
}

func TestParse_ModelsOptional(t *testing.T) {
	specs, _, err := Parse([]byte(`
agents:
  - name: opencode
    spawn: ["opencode", "acp"]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(specs[0].Models) != 0 {
		t.Errorf("specs[0].Models = %+v, want empty when omitted", specs[0].Models)
	}
}

func TestParse_DecodesPerAgentEnv(t *testing.T) {
	specs, _, err := Parse([]byte(`
agents:
  - name: claude
    spawn: ["claude", "--acp"]
    env:
      ANTHROPIC_BASE_URL: "{{ENV:CHORUS_HEADROOM_HOST_URL}}"
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got := specs[0].Env["ANTHROPIC_BASE_URL"]; got != "{{ENV:CHORUS_HEADROOM_HOST_URL}}" {
		t.Errorf("specs[0].Env[ANTHROPIC_BASE_URL] = %q, want the raw token (substitution happens later, in session.Connect)", got)
	}
}

func TestParse_EnvOptional(t *testing.T) {
	specs, _, err := Parse([]byte(`
agents:
  - name: opencode
    spawn: ["opencode", "acp"]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(specs[0].Env) != 0 {
		t.Errorf("specs[0].Env = %+v, want empty when omitted", specs[0].Env)
	}
}

func TestParse_DecodesHeadroomConfig(t *testing.T) {
	_, cfg, err := Parse([]byte(`
headroom:
  enabled: true
  image: custom/headroom:tag
  port: 9999
  mode: token
agents:
  - name: opencode
    spawn: ["opencode", "acp"]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !cfg.Headroom.EnabledOrDefault() {
		t.Error("Headroom.EnabledOrDefault() = false, want true")
	}
	if cfg.Headroom.ImageOrDefault() != "custom/headroom:tag" {
		t.Errorf("Headroom.ImageOrDefault() = %q, want the configured image", cfg.Headroom.ImageOrDefault())
	}
	if cfg.Headroom.PortOrDefault() != 9999 {
		t.Errorf("Headroom.PortOrDefault() = %d, want 9999", cfg.Headroom.PortOrDefault())
	}
	if cfg.Headroom.ModeOrDefault() != "token" {
		t.Errorf("Headroom.ModeOrDefault() = %q, want token", cfg.Headroom.ModeOrDefault())
	}
}

func TestParse_HeadroomOptional(t *testing.T) {
	_, cfg, err := Parse([]byte(`
agents:
  - name: opencode
    spawn: ["opencode", "acp"]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Headroom.EnabledOrDefault() {
		t.Error("Headroom.EnabledOrDefault() = true with no headroom: block at all, want false (opt-in)")
	}
}
