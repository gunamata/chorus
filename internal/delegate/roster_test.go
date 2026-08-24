package delegate

import (
	"strings"
	"testing"

	"chorus/internal/session"
)

func TestBuildRoster_ExcludesSelfAndUnconnectedAgents(t *testing.T) {
	specs := []session.Spec{
		{Name: "claude", CostTier: "metered", Notes: "general purpose"},
		{Name: "gemini", CostTier: "seat", Notes: "summarize/explain"},
		{Name: "opencode", CostTier: "free", Notes: "catch-all"},
	}
	// gemini never connected this run.
	conns := map[string]*session.Connection{
		"claude":   nil,
		"opencode": nil,
	}
	roster := BuildRoster(specs, conns, "claude")
	if len(roster) != 1 {
		t.Fatalf("BuildRoster() = %+v, want exactly opencode (claude excluded as self, gemini excluded as unconnected)", roster)
	}
	if roster[0].Name != "opencode" {
		t.Errorf("roster[0].Name = %q, want %q", roster[0].Name, "opencode")
	}
	if roster[0].CostTier != "free" || roster[0].Notes != "catch-all" {
		t.Errorf("roster[0] = %+v, want CostTier/Notes carried through from the spec", roster[0])
	}
}

func TestBuildRoster_PreservesFileOrder(t *testing.T) {
	specs := []session.Spec{
		{Name: "opencode"},
		{Name: "claude"},
		{Name: "gemini"},
	}
	conns := map[string]*session.Connection{
		"opencode": nil,
		"claude":   nil,
		"gemini":   nil,
	}
	roster := BuildRoster(specs, conns, "claude")
	want := []string{"opencode", "gemini"}
	if len(roster) != len(want) {
		t.Fatalf("BuildRoster() = %+v, want len %d", roster, len(want))
	}
	for i, name := range want {
		if roster[i].Name != name {
			t.Errorf("roster[%d].Name = %q, want %q (spec order not preserved)", i, roster[i].Name, name)
		}
	}
}

func TestEncodeDecodeRoster_RoundTrip(t *testing.T) {
	in := []RosterEntry{
		{Name: "claude", CostTier: "metered", Notes: "general purpose"},
		{Name: "opencode", CostTier: "free"},
	}
	encoded, err := EncodeRoster(in)
	if err != nil {
		t.Fatalf("EncodeRoster() error = %v", err)
	}
	out, err := DecodeRoster(encoded)
	if err != nil {
		t.Fatalf("DecodeRoster() error = %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("DecodeRoster() = %+v, want %d entries", out, len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("out[%d] = %+v, want %+v", i, out[i], in[i])
		}
	}
}

func TestDecodeRoster_EmptyStringIsNotAnError(t *testing.T) {
	roster, err := DecodeRoster("")
	if err != nil {
		t.Fatalf("DecodeRoster(\"\") error = %v, want nil (missing env var == no roster data, not a failure)", err)
	}
	if roster != nil {
		t.Errorf("DecodeRoster(\"\") = %+v, want nil", roster)
	}
}

func TestEncodeRoster_EmptyRosterEncodesToEmptyString(t *testing.T) {
	encoded, err := EncodeRoster(nil)
	if err != nil {
		t.Fatalf("EncodeRoster(nil) error = %v", err)
	}
	if encoded != "" {
		t.Errorf("EncodeRoster(nil) = %q, want empty string", encoded)
	}
}

func TestFormatRosterLines_IncludesCostTierAndNotes(t *testing.T) {
	out := FormatRosterLines([]RosterEntry{
		{Name: "opencode", CostTier: "free", Notes: "catch-all agent"},
	})
	if !strings.Contains(out, "opencode") {
		t.Errorf("FormatRosterLines() = %q, want the agent name present", out)
	}
	if !strings.Contains(out, "free") {
		t.Errorf("FormatRosterLines() = %q, want the cost tier present", out)
	}
	if !strings.Contains(out, "catch-all agent") {
		t.Errorf("FormatRosterLines() = %q, want the notes present", out)
	}
}
