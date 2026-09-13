package router

import (
	"strings"
	"testing"

	"chorus/internal/session"
)

func testAgents() []DecisionAgentInfo {
	return []DecisionAgentInfo{
		{Name: "claude", CostTier: "metered", Notes: "general-purpose", Models: []session.ModelInfo{
			{ID: "claude-opus-4-8", Label: "Opus", WhenToUse: "hard problems"},
			{ID: "claude-haiku-4-5-20251001", Label: "Haiku", WhenToUse: "mechanical work"},
		}},
		{Name: "opencode", CostTier: "free", Notes: "free by default"},
	}
}

func TestBuildDecisionPrompt_MentionsAgentsModelsAndPrompt(t *testing.T) {
	p := BuildDecisionPrompt(testAgents(), "opencode", "claude did X earlier", "now do Y", false)
	for _, want := range []string{
		"automated routing-decision request from chorus itself",
		"not a message from the user",
		"do not use any tools",
		"claude", "opencode",
		"claude-opus-4-8", "claude-haiku-4-5-20251001",
		"claude did X earlier",
		"now do Y",
	} {
		if !strings.Contains(strings.ToLower(p), strings.ToLower(want)) {
			t.Errorf("BuildDecisionPrompt() missing %q in:\n%s", want, p)
		}
	}
}

func TestBuildDecisionPrompt_OmitsContextSectionWhenEmpty(t *testing.T) {
	p := BuildDecisionPrompt(testAgents(), "opencode", "", "hello", false)
	if strings.Contains(p, "Recent activity") {
		t.Fatalf("BuildDecisionPrompt() with empty contextText still included a context section:\n%s", p)
	}
}

func TestBuildDecisionPrompt_AnonymizesPromptAndContextWhenTrue(t *testing.T) {
	p := BuildDecisionPrompt(testAgents(), "opencode", "earlier I asked about jane.doe@example.com", "email me at jane.doe@example.com", true)
	if strings.Contains(p, "jane.doe@example.com") {
		t.Fatalf("BuildDecisionPrompt(anonymize=true) still contains the raw email:\n%s", p)
	}
	if !strings.Contains(p, "[redacted-email]") {
		t.Fatalf("BuildDecisionPrompt(anonymize=true) missing [redacted-email]:\n%s", p)
	}
}

func TestBuildDecisionPrompt_LeavesSensitiveTextAloneWhenFalse(t *testing.T) {
	p := BuildDecisionPrompt(testAgents(), "opencode", "", "email me at jane.doe@example.com", false)
	if !strings.Contains(p, "jane.doe@example.com") {
		t.Fatalf("BuildDecisionPrompt(anonymize=false) redacted the prompt, want it untouched:\n%s", p)
	}
}

func TestParseDecision_CleanJSON(t *testing.T) {
	d, err := ParseDecision(`{"agent": "claude", "model": "claude-opus-4-8", "reason": "hard task"}`, testAgents())
	if err != nil {
		t.Fatalf("ParseDecision() error = %v", err)
	}
	if d.Agent != "claude" || d.Model != "claude-opus-4-8" || d.Reason != "hard task" {
		t.Fatalf("ParseDecision() = %+v, want claude/claude-opus-4-8/hard task", d)
	}
}

func TestParseDecision_MarkdownFencedJSON(t *testing.T) {
	raw := "```json\n{\"agent\": \"opencode\", \"model\": \"\", \"reason\": \"simple\"}\n```"
	d, err := ParseDecision(raw, testAgents())
	if err != nil {
		t.Fatalf("ParseDecision() error = %v", err)
	}
	if d.Agent != "opencode" {
		t.Fatalf("ParseDecision() = %+v, want opencode", d)
	}
}

func TestParseDecision_LeadingProseBeforeJSON(t *testing.T) {
	raw := "Sure, here's my decision: {\"agent\": \"claude\", \"model\": \"\", \"reason\": \"needs judgment\"}"
	d, err := ParseDecision(raw, testAgents())
	if err != nil {
		t.Fatalf("ParseDecision() error = %v", err)
	}
	if d.Agent != "claude" {
		t.Fatalf("ParseDecision() = %+v, want claude", d)
	}
}

func TestParseDecision_BraceInsideStringValueDoesNotConfuseBoundary(t *testing.T) {
	raw := `{"agent": "claude", "model": "", "reason": "handles cases like {foo: bar} in code"}`
	d, err := ParseDecision(raw, testAgents())
	if err != nil {
		t.Fatalf("ParseDecision() error = %v", err)
	}
	if d.Agent != "claude" || d.Reason == "" {
		t.Fatalf("ParseDecision() = %+v, want claude with reason preserved", d)
	}
}

func TestParseDecision_UnknownAgentErrors(t *testing.T) {
	_, err := ParseDecision(`{"agent": "nonexistent", "model": "", "reason": "x"}`, testAgents())
	if err == nil {
		t.Fatal("ParseDecision() error = nil, want an error for an unknown agent")
	}
}

func TestParseDecision_UnknownModelErrors(t *testing.T) {
	_, err := ParseDecision(`{"agent": "claude", "model": "gpt-5", "reason": "x"}`, testAgents())
	if err == nil {
		t.Fatal("ParseDecision() error = nil, want an error for a model claude doesn't declare")
	}
}

func TestParseDecision_EmptyModelAlwaysValid(t *testing.T) {
	// opencode declares no models at all — an empty Model must still validate.
	d, err := ParseDecision(`{"agent": "opencode", "model": "", "reason": "x"}`, testAgents())
	if err != nil {
		t.Fatalf("ParseDecision() error = %v, want empty model to always be valid", err)
	}
	if d.Agent != "opencode" {
		t.Fatalf("ParseDecision() = %+v, want opencode", d)
	}
}

func TestParseDecision_NoJSONObjectErrors(t *testing.T) {
	_, err := ParseDecision("I don't know, maybe claude?", testAgents())
	if err == nil {
		t.Fatal("ParseDecision() error = nil, want an error when there's no JSON object at all")
	}
}

func TestParseDecision_MalformedJSONErrorsWithoutPanic(t *testing.T) {
	_, err := ParseDecision(`{"agent": "claude", "model": `, testAgents())
	if err == nil {
		t.Fatal("ParseDecision() error = nil, want an error for truncated/malformed JSON")
	}
}

func TestParseDecision_EmptyStringErrorsWithoutPanic(t *testing.T) {
	_, err := ParseDecision("", testAgents())
	if err == nil {
		t.Fatal("ParseDecision() error = nil, want an error for an empty reply")
	}
}
