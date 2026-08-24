package sessionstore

import (
	"path/filepath"
	"testing"
)

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), ".chorus", "sessions.json"))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for a missing file", err)
	}
	if _, ok := s.Get("claude"); ok {
		t.Fatal("Get() on an empty store returned ok=true")
	}
}

func TestSetThenGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".chorus", "sessions.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := s.Set("claude", "sess-123"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	id, ok := s.Get("claude")
	if !ok || id != "sess-123" {
		t.Fatalf("Get() = (%q, %v), want (sess-123, true)", id, ok)
	}
}

func TestSet_PersistsAcrossLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".chorus", "sessions.json")

	s1, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := s1.Set("claude", "sess-abc"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := s1.Set("opencode", "sess-xyz"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	s2, err := Load(path)
	if err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	if id, ok := s2.Get("claude"); !ok || id != "sess-abc" {
		t.Errorf(`Get("claude") = (%q, %v), want (sess-abc, true)`, id, ok)
	}
	if id, ok := s2.Get("opencode"); !ok || id != "sess-xyz" {
		t.Errorf(`Get("opencode") = (%q, %v), want (sess-xyz, true)`, id, ok)
	}
}

func TestClear_RemovesEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".chorus", "sessions.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := s.Set("claude", "sess-123"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := s.Clear("claude"); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if _, ok := s.Get("claude"); ok {
		t.Fatal("Get() after Clear() returned ok=true")
	}

	// Clearing an already-absent agent is a no-op, not an error.
	if err := s.Clear("gemini"); err != nil {
		t.Fatalf("Clear() on a never-set agent error = %v, want nil", err)
	}
}

func TestClear_PersistsAcrossLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".chorus", "sessions.json")
	s1, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	_ = s1.Set("claude", "sess-123")
	_ = s1.Clear("claude")

	s2, err := Load(path)
	if err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	if _, ok := s2.Get("claude"); ok {
		t.Fatal("cleared entry reappeared after reload")
	}
}
