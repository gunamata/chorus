package delegate

import (
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

// Collectors accumulates streamed reply text for delegation sub-sessions
// so a Hub call can retrieve a sub-session's full reply once its turn
// completes — without that reply (or anything else from the sub-session:
// thoughts, tool calls, plans) also appearing in the interactive terminal.
// §11 sub-sessions are a private "digest" channel, not something the user
// watches directly.
//
// This is also how main.go's output loop knows whether a given
// session/update belongs to a delegation sub-session at all: IsRegistered
// answers that for every update kind, not just the text-bearing ones.
type Collectors struct {
	mu   sync.Mutex
	bufs map[acp.SessionId]*strings.Builder
}

func NewCollectors() *Collectors {
	return &Collectors{bufs: make(map[acp.SessionId]*strings.Builder)}
}

// Register starts collecting for sessionID.
func (c *Collectors) Register(sessionID acp.SessionId) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bufs[sessionID] = &strings.Builder{}
}

// Append appends text for sessionID if it's currently registered, and
// reports whether it was — the caller uses this to decide whether an
// agent_message_chunk belongs to a delegation sub-session.
func (c *Collectors) Append(sessionID acp.SessionId, text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.bufs[sessionID]
	if ok {
		b.WriteString(text)
	}
	return ok
}

// IsRegistered reports whether sessionID is currently being collected,
// without appending anything — used for update kinds that carry no reply
// text (tool calls, plans, usage updates, ...) but still belong to a
// delegation sub-session and so still shouldn't render interactively.
func (c *Collectors) IsRegistered(sessionID acp.SessionId) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.bufs[sessionID]
	return ok
}

// Collect returns the accumulated text and stops collecting for
// sessionID.
func (c *Collectors) Collect(sessionID acp.SessionId) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.bufs[sessionID]
	delete(c.bufs, sessionID)
	if !ok {
		return ""
	}
	return b.String()
}
