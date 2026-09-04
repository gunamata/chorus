// Package sessionstore persists each agent's main-interactive-session ID
// across chorus runs, so a resumed run can call ACP's session/load
// instead of always starting fresh. Sessions are scoped per-project
// because ACP's session/load itself requires the request's cwd to match
// the session's original cwd — a session ID is only meaningful in the
// directory it was created in.
//
// As of 2026-09-03, the store itself lives centrally under
// ~/.chorus/projects/<slug>/ (ProjectDir) rather than a per-project
// ./.chorus/ directory — the per-project SCOPING of session IDs hasn't
// changed (still keyed by cwd, for the reason above), only WHERE that
// per-project state is physically kept on disk, so a project's .git-style
// clutter doesn't include a chorus-owned directory and every project's
// state is visible/cleanable from one place. Pre-existing ./.chorus/
// directories from before this change are simply orphaned, not migrated
// — chorus never reads them again, and a "session not found" style fresh
// restart is already the documented fallback behavior for a missing or
// stale session anyway (see resumeOrNewSession in main.go).
//
// Only the main interactive session per agent is ever stored here.
// Delegation sub-sessions (§11) are intentionally short-lived and never
// touch this store.
//
// Alongside each session ID, the store also tracks whether that agent has
// ever received the one-time delegation briefing (main.go's
// buildBriefingText) — added after a documented gap (CLAUDE.md's
// delegation-maximization plan): the briefing used to be gated purely on
// "was this session resumed," which meant a session that existed before a
// 2nd agent ever connected — the exact shape of an ordinary chorus
// session that's been running a while — would never receive it, no matter
// how many later runs happened with delegation fully possible. Tracking
// "briefed" as its own persistent fact, independent of resume status,
// fixes that: any session whose stored entry doesn't yet say briefed gets
// the briefing on its next run, exactly once, then never again.
package sessionstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// HomeDir returns chorus's centralized per-user state root (~/.chorus),
// overridable via CHORUS_HOME (mainly so tests never touch a real home
// directory, but also a legitimate escape hatch for anyone who wants
// chorus's state somewhere else entirely, e.g. a non-default drive).
func HomeDir() (string, error) {
	if h := os.Getenv("CHORUS_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".chorus"), nil
}

var slugUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// ProjectDir returns the directory under HomeDir that holds cwd's session
// store, agent stderr logs, and inbound-image cache
// (~/.chorus/projects/<slug>). A pure path computation — it does not
// create the directory; callers (Load's caller via os.MkdirAll in save,
// main.go for logs/images) create it exactly as they did when this lived
// under a project-local ./.chorus/.
//
// <slug> is cwd's base name (for a directory listing a human can actually
// recognize) plus a short hash of the full absolute path (for collision
// safety — two different projects that happen to share a base name, e.g.
// two separate "chorus" checkouts in different places, must never share
// state). Windows paths are case-insensitive, so the hash is computed on
// a lower-cased path there specifically so "C:\Chorus" and "c:\chorus"
// — the same directory as far as the filesystem is concerned — hash
// identically instead of silently getting two unrelated state dirs.
func ProjectDir(cwd string) (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	// Both the hash AND the human-readable base name are derived from this
	// same case-folded form on Windows — deriving only the hash from it
	// (an earlier version of this function) still let "C:\Chorus" and
	// "c:\chorus" collide on the hash but land in two different-looking
	// sibling directories ("Chorus-<hash>" vs "chorus-<hash>"), defeating
	// the whole point. Caught by TestProjectDir_CaseInsensitiveOnWindows
	// before shipping.
	slugInput := abs
	if runtime.GOOS == "windows" {
		slugInput = strings.ToLower(abs)
	}
	sum := sha256.Sum256([]byte(slugInput))
	hash := hex.EncodeToString(sum[:6]) // 12 hex chars — plenty to avoid collisions here
	base := slugUnsafe.ReplaceAllString(filepath.Base(slugInput), "_")
	if base == "" || base == "_" {
		base = "project"
	}
	return filepath.Join(home, "projects", base+"-"+hash), nil
}

// entry is one agent's stored state. Briefed persists independently of
// SessionID — a session resumed across many runs (--resume) keeps its
// briefing status, but a genuinely fresh session (new agent, the default
// no-flag run, or a stale ID that was rejected and replaced) must have it
// reset, since empty
// history was never actually shown the earlier briefing regardless of
// what a prior session under the same agent name received — see
// ResetBriefed.
type entry struct {
	SessionID string `json:"session_id"`
	Briefed   bool   `json:"briefed"`
}

// UnmarshalJSON accepts both the current object form and the plain
// bare-string form every .chorus/sessions.json written before briefing
// tracking existed. A legacy entry decodes with Briefed: false, which is
// exactly correct: a session that predates this feature has, by
// definition, never received the briefing chorus now knows to check for.
func (e *entry) UnmarshalJSON(b []byte) error {
	var plain string
	if err := json.Unmarshal(b, &plain); err == nil {
		e.SessionID = plain
		e.Briefed = false
		return nil
	}
	type alias entry
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*e = entry(a)
	return nil
}

// Store is agent name -> stored entry, backed by a JSON file.
type Store struct {
	path string
	data map[string]entry
}

// Load reads path if it exists. A missing file is not an error — it
// yields an empty Store, under which every agent starts a fresh session
// (today's behavior, unchanged for anyone who never uses this feature).
func Load(path string) (*Store, error) {
	s := &Store{path: path, data: make(map[string]entry)}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, err
	}
	return s, nil
}

// Get returns the saved session ID for agent, if any.
func (s *Store) Get(agent string) (string, bool) {
	e, ok := s.data[agent]
	if !ok || e.SessionID == "" {
		return "", false
	}
	return e.SessionID, true
}

// Set records agent's current session ID and writes the store to disk
// immediately — a session ID is a durable handle to history the agent
// already has, valid for the life of that history, so there's nothing
// to batch or debounce. Leaves Briefed untouched — see ResetBriefed for
// the fresh-session case that needs to clear it.
func (s *Store) Set(agent, sessionID string) error {
	e := s.data[agent]
	e.SessionID = sessionID
	s.data[agent] = e
	return s.save()
}

// Clear removes agent's saved session ID (used when a saved ID turns out
// to be stale — the agent rejected LoadSession — so the next run doesn't
// keep retrying a dead session). Removes the whole entry, Briefed
// included: the session this was tracking no longer exists.
func (s *Store) Clear(agent string) error {
	if _, ok := s.data[agent]; !ok {
		return nil
	}
	delete(s.data, agent)
	return s.save()
}

// Briefed reports whether agent has ever received the delegation
// briefing in any past run, independent of its current session ID, so a
// session resumed across many runs is only ever briefed once.
func (s *Store) Briefed(agent string) bool {
	return s.data[agent].Briefed
}

// MarkBriefed records that agent's current session has now received the
// delegation briefing, persisted immediately so a crash or restart right
// after doesn't re-send it.
func (s *Store) MarkBriefed(agent string) error {
	e := s.data[agent]
	e.Briefed = true
	s.data[agent] = e
	return s.save()
}

// ResetBriefed clears agent's briefing status — call whenever a
// genuinely fresh session is created (resumeOrNewSession returned
// resumed=false), most importantly for the default no-flag run (every
// run is fresh unless --resume is passed): a fresh session's history is
// empty, so whatever briefing status a PRIOR session under the same
// agent name reached is no longer true for this one, and leaving it set
// would silently skip briefing a session that has actually never seen it.
func (s *Store) ResetBriefed(agent string) error {
	e, ok := s.data[agent]
	if !ok || !e.Briefed {
		return nil
	}
	e.Briefed = false
	s.data[agent] = e
	return s.save()
}

func (s *Store) save() error {
	// Owner-only permissions (chorus-spec.md §0's 2026-08-22 audit):
	// session IDs are bearer-token-like handles to an agent's own
	// conversation history, not ordinary project files — 0o644/0o755
	// would leave them world-readable on any multi-user Unix/macOS
	// machine.
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}
