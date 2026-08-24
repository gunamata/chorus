package router

import (
	"testing"

	"chorus/internal/policy"
)

func testRouting() policy.Routing {
	return policy.Routing{
		Default: "opencode",
		Rules: []policy.Rule{
			{Match: []string{"fix", "implement", "refactor", "debug", "architecture"}, Agent: "claude"},
			{Match: []string{"summarize", "explain", "list", "scan"}, Agent: "gemini"},
		},
	}
}

func TestChoose_MatchesRule(t *testing.T) {
	d := Choose(testRouting(), "please fix the login bug")
	if !d.Matched || d.Agent != "claude" {
		t.Fatalf("Choose() = %+v, want matched claude", d)
	}
}

func TestChoose_CaseInsensitive(t *testing.T) {
	d := Choose(testRouting(), "FIX this now")
	if !d.Matched || d.Agent != "claude" {
		t.Fatalf("Choose() = %+v, want matched claude", d)
	}
}

// Regression test: Choose used to match keywords as raw substrings, so
// "fix" matched inside "prefix"/"suffix" and "list" matched inside
// "checklist" — misrouting prompts that never used the word as a word.
func TestChoose_WordBoundary_NoFalsePositive(t *testing.T) {
	d := Choose(testRouting(), "add a prefix and suffix to the string, then update the checklist")
	if d.Matched {
		t.Fatalf("Choose() = %+v, expected no match (word-boundary false positive on prefix/suffix/checklist)", d)
	}
	if d.Agent != "opencode" {
		t.Fatalf("Agent = %q, want fallback to default opencode", d.Agent)
	}
}

func TestChoose_FallsBackToDefault(t *testing.T) {
	d := Choose(testRouting(), "give me a random word")
	if d.Matched {
		t.Fatalf("Choose() = %+v, expected no rule to match", d)
	}
	if d.Agent != "opencode" {
		t.Fatalf("Agent = %q, want default opencode", d.Agent)
	}
}

func TestChoose_FirstRuleWins(t *testing.T) {
	r := policy.Routing{
		Default: "x",
		Rules: []policy.Rule{
			{Match: []string{"foo"}, Agent: "a"},
			{Match: []string{"foo"}, Agent: "b"},
		},
	}
	d := Choose(r, "foo bar")
	if d.Agent != "a" {
		t.Fatalf("Agent = %q, want first matching rule (a)", d.Agent)
	}
}

func TestChoose_EmptyMatchEntriesIgnored(t *testing.T) {
	r := policy.Routing{
		Default: "opencode",
		Rules: []policy.Rule{
			{Match: []string{"", "fix"}, Agent: "claude"},
		},
	}
	d := Choose(r, "fix it")
	if !d.Matched || d.Agent != "claude" {
		t.Fatalf("Choose() = %+v, want matched claude despite an empty match entry", d)
	}
}

func TestChoose_NoRulesNoDefault(t *testing.T) {
	d := Choose(policy.Routing{}, "anything")
	if d.Matched {
		t.Fatalf("Choose() = %+v, expected no match with no rules", d)
	}
	if d.Agent != "" {
		t.Fatalf("Agent = %q, want empty with no default configured", d.Agent)
	}
}
