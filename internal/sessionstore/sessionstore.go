// Package sessionstore persists each agent's main-interactive-session ID
// across chorus runs, in a project-local file, so a resumed run can call
// ACP's session/load instead of always starting fresh. Sessions are
// scoped per-project because ACP's session/load itself requires the
// request's cwd to match the session's original cwd — a session ID is
// only meaningful in the directory it was created in.
//
// Only the main interactive session per agent is ever stored here.
// Delegation sub-sessions (§11) are intentionally short-lived and never
// touch this store.
package sessionstore

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Store is agent name -> last known session ID, backed by a JSON file.
type Store struct {
	path string
	data map[string]string
}

// Load reads path if it exists. A missing file is not an error — it
// yields an empty Store, under which every agent starts a fresh session
// (today's behavior, unchanged for anyone who never uses this feature).
func Load(path string) (*Store, error) {
	s := &Store{path: path, data: make(map[string]string)}
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
	id, ok := s.data[agent]
	return id, ok
}

// Set records agent's current session ID and writes the store to disk
// immediately — a session ID is a durable handle to history the agent
// already has, valid for the life of that history, so there's nothing
// to batch or debounce.
func (s *Store) Set(agent, sessionID string) error {
	s.data[agent] = sessionID
	return s.save()
}

// Clear removes agent's saved session ID (used when a saved ID turns out
// to be stale — the agent rejected LoadSession — so the next run doesn't
// keep retrying a dead session).
func (s *Store) Clear(agent string) error {
	if _, ok := s.data[agent]; !ok {
		return nil
	}
	delete(s.data, agent)
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
