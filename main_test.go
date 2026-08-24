package main

import (
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
