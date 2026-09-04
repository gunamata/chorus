package sessionstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// --- delegation-briefing tracking (the fix for CLAUDE.md's documented
// "a session resumed from before a 2nd agent existed never gets the
// briefing" gap) -----------------------------------------------------

func TestBriefed_FalseUntilMarked(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".chorus", "sessions.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	_ = s.Set("claude", "sess-123")
	if s.Briefed("claude") {
		t.Fatal("Briefed() = true for a session that was never marked briefed")
	}
	if err := s.MarkBriefed("claude"); err != nil {
		t.Fatalf("MarkBriefed() error = %v", err)
	}
	if !s.Briefed("claude") {
		t.Fatal("Briefed() = false right after MarkBriefed()")
	}
}

func TestBriefed_PersistsAcrossLoadIndependentOfResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".chorus", "sessions.json")
	s1, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	_ = s1.Set("claude", "sess-123")
	_ = s1.MarkBriefed("claude")

	// A later run resumes the same session (Set is called again with the
	// same ID, exactly as main.go does after a successful LoadSession) —
	// Briefed must survive that untouched.
	_ = s1.Set("claude", "sess-123")
	if !s1.Briefed("claude") {
		t.Fatal("Set() cleared Briefed — session-ID bookkeeping must not affect briefing status")
	}

	s2, err := Load(path)
	if err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	if !s2.Briefed("claude") {
		t.Fatal("Briefed() = false after reload, want the marked status to persist")
	}
}

func TestResetBriefed_ClearsStatusForAFreshSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".chorus", "sessions.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	_ = s.Set("claude", "sess-123")
	_ = s.MarkBriefed("claude")

	// e.g. --fresh: a brand-new session under the same agent name, whose
	// empty history was never actually shown the earlier briefing.
	if err := s.ResetBriefed("claude"); err != nil {
		t.Fatalf("ResetBriefed() error = %v", err)
	}
	if s.Briefed("claude") {
		t.Fatal("Briefed() = true after ResetBriefed()")
	}
}

func TestClear_AlsoRemovesBriefedStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".chorus", "sessions.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	_ = s.Set("claude", "sess-123")
	_ = s.MarkBriefed("claude")
	_ = s.Clear("claude")
	if s.Briefed("claude") {
		t.Fatal("Briefed() = true after Clear() — a cleared (stale/rejected) session's entry should be gone entirely")
	}
}

// TestLoad_AcceptsLegacyPlainStringFormat covers every .chorus/
// sessions.json written before briefing tracking existed — a bare
// agent -> session-ID string map, with no briefing information at all.
// Decoding one of those entries must yield Briefed: false, since a
// session that predates this feature has, by definition, never received
// the briefing — this is exactly what makes already-resumed sessions
// (the common case for anyone upgrading chorus mid-project) pick up the
// briefing on their very next run instead of silently never getting it.
func TestLoad_AcceptsLegacyPlainStringFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(path, []byte(`{"claude":"sess-legacy-123"}`), 0o600); err != nil {
		t.Fatalf("failed to write legacy fixture: %v", err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want a legacy plain-string entry to decode cleanly", err)
	}
	id, ok := s.Get("claude")
	if !ok || id != "sess-legacy-123" {
		t.Fatalf("Get(\"claude\") = (%q, %v), want (sess-legacy-123, true)", id, ok)
	}
	if s.Briefed("claude") {
		t.Fatal("Briefed() = true for a legacy entry that predates briefing tracking, want false")
	}
}

// --- HomeDir / ProjectDir (2026-09-03 centralized-state move) ---------

func TestHomeDir_DefaultsUnderUserHome(t *testing.T) {
	t.Setenv("CHORUS_HOME", "")
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no user home dir available in this environment: %v", err)
	}
	got, err := HomeDir()
	if err != nil {
		t.Fatalf("HomeDir() error = %v", err)
	}
	want := filepath.Join(realHome, ".chorus")
	if got != want {
		t.Fatalf("HomeDir() = %q, want %q", got, want)
	}
}

func TestHomeDir_RespectsOverride(t *testing.T) {
	t.Setenv("CHORUS_HOME", filepath.Join(t.TempDir(), "custom-chorus-home"))
	got, err := HomeDir()
	if err != nil {
		t.Fatalf("HomeDir() error = %v", err)
	}
	if got != os.Getenv("CHORUS_HOME") {
		t.Fatalf("HomeDir() = %q, want it to return CHORUS_HOME verbatim", got)
	}
}

func TestProjectDir_StableForTheSameCwd(t *testing.T) {
	t.Setenv("CHORUS_HOME", t.TempDir())
	cwd := t.TempDir()
	a, err := ProjectDir(cwd)
	if err != nil {
		t.Fatalf("ProjectDir() error = %v", err)
	}
	b, err := ProjectDir(cwd)
	if err != nil {
		t.Fatalf("ProjectDir() error = %v", err)
	}
	if a != b {
		t.Fatalf("ProjectDir(%q) = %q then %q, want the same path both times", cwd, a, b)
	}
}

func TestProjectDir_DiffersForDifferentCwds(t *testing.T) {
	t.Setenv("CHORUS_HOME", t.TempDir())
	a, err := ProjectDir(t.TempDir())
	if err != nil {
		t.Fatalf("ProjectDir() error = %v", err)
	}
	b, err := ProjectDir(t.TempDir())
	if err != nil {
		t.Fatalf("ProjectDir() error = %v", err)
	}
	if a == b {
		t.Fatalf("ProjectDir() returned the same path (%q) for two different cwds, want distinct state dirs", a)
	}
}

func TestProjectDir_UnderHomeDirProjectsSubdir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHORUS_HOME", home)
	got, err := ProjectDir(t.TempDir())
	if err != nil {
		t.Fatalf("ProjectDir() error = %v", err)
	}
	if filepath.Dir(got) != filepath.Join(home, "projects") {
		t.Fatalf("ProjectDir() = %q, want it directly under %q", got, filepath.Join(home, "projects"))
	}
}

func TestProjectDir_SlugIsReadableAndFilesystemSafe(t *testing.T) {
	t.Setenv("CHORUS_HOME", t.TempDir())
	cwd := filepath.Join(t.TempDir(), "my project (v2)")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatalf("failed to create fixture dir: %v", err)
	}
	got, err := ProjectDir(cwd)
	if err != nil {
		t.Fatalf("ProjectDir() error = %v", err)
	}
	slug := filepath.Base(got)
	if !strings.HasPrefix(slug, "my_project_v2_-") {
		t.Fatalf("slug = %q, want a sanitized \"my_project_v2_-<hash>\" prefix", slug)
	}
	if strings.ContainsAny(slug, " ()") {
		t.Fatalf("slug = %q, still contains characters unsafe on some filesystems", slug)
	}
}

func TestProjectDir_CaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitivity only applies on Windows, where the filesystem itself is case-insensitive")
	}
	t.Setenv("CHORUS_HOME", t.TempDir())
	lower, err := ProjectDir(`C:\Chorus\Fixture`)
	if err != nil {
		t.Fatalf("ProjectDir() error = %v", err)
	}
	upper, err := ProjectDir(`c:\chorus\fixture`)
	if err != nil {
		t.Fatalf("ProjectDir() error = %v", err)
	}
	if lower != upper {
		t.Fatalf("ProjectDir() differed by case (%q vs %q), want the same slug since Windows treats these as the same path", lower, upper)
	}
}
