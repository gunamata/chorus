package tui

import (
	"os"
	"path/filepath"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// --- slash-command lookup -------------------------------------------

func TestCommandName(t *testing.T) {
	cases := map[string]string{
		"/compact":          "compact",
		"/compact do it":    "compact",
		"/code-review file": "code-review",
		"not-a-command":     "not-a-command", // no leading slash trimmed, but still "the first word"
	}
	for line, want := range cases {
		if got := commandName(line); got != want {
			t.Errorf("commandName(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestCommandOwners_SingleOwner(t *testing.T) {
	commands := map[string][]acp.AvailableCommand{
		"claude":   {{Name: "compact"}},
		"opencode": {{Name: "init"}},
	}
	owners := commandOwners(commands, "/compact do it")
	if len(owners) != 1 || owners[0] != "claude" {
		t.Fatalf("commandOwners() = %v, want [claude]", owners)
	}
}

func TestCommandOwners_CaseInsensitive(t *testing.T) {
	commands := map[string][]acp.AvailableCommand{
		"claude": {{Name: "Compact"}},
	}
	owners := commandOwners(commands, "/compact")
	if len(owners) != 1 || owners[0] != "claude" {
		t.Fatalf("commandOwners() = %v, want [claude]", owners)
	}
}

func TestCommandOwners_MultipleOwnersSorted(t *testing.T) {
	commands := map[string][]acp.AvailableCommand{
		"opencode": {{Name: "review"}},
		"claude":   {{Name: "review"}},
	}
	owners := commandOwners(commands, "/review")
	if len(owners) != 2 || owners[0] != "claude" || owners[1] != "opencode" {
		t.Fatalf("commandOwners() = %v, want [claude opencode] (sorted)", owners)
	}
}

func TestCommandOwners_NoOwner(t *testing.T) {
	commands := map[string][]acp.AvailableCommand{
		"claude": {{Name: "compact"}},
	}
	if owners := commandOwners(commands, "/nonexistent"); owners != nil {
		t.Fatalf("commandOwners() = %v, want nil", owners)
	}
}

func TestFormatCommands_ListsPerAgentSorted(t *testing.T) {
	commands := map[string][]acp.AvailableCommand{
		"claude": {
			{Name: "zzz", Description: "last"},
			{Name: "aaa", Description: "first"},
		},
	}
	out := formatCommands(commands)
	iAAA := indexOf(out, "/aaa")
	iZZZ := indexOf(out, "/zzz")
	if iAAA == -1 || iZZZ == -1 || iAAA > iZZZ {
		t.Fatalf("formatCommands() didn't list commands sorted within an agent; got %q", out)
	}
}

func TestFormatCommands_EmptyIsFriendly(t *testing.T) {
	out := formatCommands(map[string][]acp.AvailableCommand{})
	if out == "" {
		t.Fatal("formatCommands() on an empty map returned empty string, want a friendly message")
	}
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// --- image attachments -------------------------------------------

func TestExtractAttachments_NoAttachments(t *testing.T) {
	clean, images := extractAttachments("just plain text, no @ tokens here")
	if len(images) != 0 {
		t.Fatalf("images = %v, want none", images)
	}
	if clean != "just plain text, no @ tokens here" {
		t.Fatalf("clean = %q, want unchanged text", clean)
	}
}

func TestExtractAttachments_ValidImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, []byte("fake-png"), 0o644); err != nil {
		t.Fatal(err)
	}

	clean, images := extractAttachments("look at @" + path + " please")
	if len(images) != 1 {
		t.Fatalf("images = %v, want exactly 1", images)
	}
	if images[0].Image == nil || images[0].Image.MimeType != "image/png" {
		t.Fatalf("images[0] = %+v, want an image/png block", images[0])
	}
	if clean == "" || containsPath(clean, path) {
		t.Fatalf("clean = %q, want the @path token removed", clean)
	}
}

func TestExtractAttachments_MissingFileLeavesTokenInPlace(t *testing.T) {
	clean, images := extractAttachments("look at @does-not-exist.png please")
	if len(images) != 0 {
		t.Fatalf("images = %v, want none for a missing file", images)
	}
	if !containsPath(clean, "does-not-exist.png") {
		t.Fatalf("clean = %q, want the unresolvable token left in place", clean)
	}
}

func TestExtractAttachments_NonImageExtensionIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, images := extractAttachments("see @" + path)
	if len(images) != 0 {
		t.Fatalf("images = %v, want none — .txt isn't an image extension", images)
	}
	if !containsPath(clean, path) {
		t.Fatalf("clean = %q, want the token left untouched", clean)
	}
}

func TestBuildPromptBlocks_TextOnly(t *testing.T) {
	blocks := buildPromptBlocks("hello world")
	if len(blocks) != 1 || blocks[0].Text == nil || blocks[0].Text.Text != "hello world" {
		t.Fatalf("buildPromptBlocks() = %+v, want a single text block", blocks)
	}
}

func TestBuildPromptBlocks_WithAttachment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, []byte("fake-png"), 0o644); err != nil {
		t.Fatal(err)
	}

	blocks := buildPromptBlocks("describe @" + path)
	if len(blocks) != 2 {
		t.Fatalf("buildPromptBlocks() = %+v, want [image, text]", blocks)
	}
	if blocks[0].Image == nil {
		t.Fatalf("blocks[0] = %+v, want an image block first", blocks[0])
	}
	if blocks[1].Text == nil || blocks[1].Text.Text != "describe" {
		t.Fatalf("blocks[1] = %+v, want text block \"describe\"", blocks[1])
	}
}

func containsPath(s, substr string) bool {
	return indexOf(s, substr) != -1
}
