//go:build claude_headroom_live

// Requires: a real Docker daemon (to run internal/headroom's proxy
// container), network access, and an authenticated `claude` CLI
// subscription (the claude-agent-acp subprocess this spawns shells out to
// the same auth state the real `claude` CLI uses). Excluded from the
// normal `go test ./...` run — see internal/headroom/live_test.go's own
// doc comment for the same reasoning.
//
// Run explicitly: go test ./internal/session/... -tags claude_headroom_live -v -run TestLive_ClaudeThroughHeadroom -timeout 120s
package session

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"chorus/internal/bus"
	"chorus/internal/headroom"
	"chorus/internal/policy"
)

// removeDirRetry deletes dir, retrying past a transient Windows file-lock
// error — found live (2026-09): Connection.Close()'s Kill() only reaches
// the immediate child (`npx`), not the actual Node.js process npx spawns
// underneath it (a real, pre-existing, Headroom-unrelated Windows process-
// tree concern worth its own follow-up), so that grandchild can still be
// mid-exit — holding a lock on its own cwd — for up to a couple of
// seconds after Close() returns. This is a test-cleanup concern only:
// Prompt() had already returned successfully with a real reply well
// before this runs, so it doesn't affect what the test actually proves.
func removeDirRetry(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("cleanup: RemoveAll(%s): %v", dir, err)
		}
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Logf("cleanup: RemoveAll(%s) still failing after retries: %v (harmless — OS temp dir, not the feature under test)", dir, err)
}

// TestLive_ClaudeThroughHeadroom answers the single most load-bearing
// unverified claim in this integration (CLAUDE.md/README's Known
// limitations): does claude-agent-acp — the ACP adapter chorus actually
// spawns, NOT the bare `claude` CLI — honor ANTHROPIC_BASE_URL the same
// documented way the CLI does? Deliberately bypasses internal/tui's
// bubbletea TUI entirely (not drivable headlessly in this environment —
// no tmux) and talks to session.Connect/NewSession/Prompt directly,
// which is a MORE precise test of this specific question than driving
// the UI would be: it isolates exactly the variable in question (does
// the spawned subprocess's env redirect the request) with nothing else
// in between.
func TestLive_ClaudeThroughHeadroom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	proxy, err := headroom.Start(ctx, headroom.Config{})
	if err != nil {
		t.Fatalf("headroom.Start() error = %v", err)
	}
	defer func() {
		if err := proxy.Stop(); err != nil {
			t.Errorf("proxy.Stop() error = %v", err)
		}
	}()

	cwd, err := os.MkdirTemp("", "chorus-headroom-live-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer removeDirRetry(t, cwd)

	spec := Spec{
		Name:    "claude",
		Command: "npx",
		Args:    []string{"-y", "@agentclientprotocol/claude-agent-acp"},
		Env:     map[string]string{"ANTHROPIC_BASE_URL": proxy.HostURL()},
	}

	outputCh := make(chan bus.Update, 64)
	permCh := make(chan bus.PermissionRequest, 4)

	conn, err := Connect(ctx, spec, cwd, outputCh, permCh, policy.Policy{}, policy.Delegation{}, nil, os.Stderr)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer conn.Close()

	sess, err := conn.NewSession(ctx, cwd, nil)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	if err := sess.Prompt(ctx, `Say the single word "banana" and nothing else.`); err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}

	// Drain whatever streamed back, non-blocking, purely to log it for a
	// human reading -v output — completion is already proven by Prompt()
	// having returned without error (concurrency invariant #1: it blocks
	// for the whole turn).
	var reply strings.Builder
drain:
	for {
		select {
		case u := <-outputCh:
			if chunk := u.Notification.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
				reply.WriteString(chunk.Content.Text.Text)
			}
		default:
			break drain
		}
	}
	t.Logf("claude replied: %q", reply.String())

	resp, err := http.Get(proxy.HostURL() + "/stats")
	if err != nil {
		t.Fatalf("GET %s/stats: %v", proxy.HostURL(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/stats status = %d, want 200", proxy.HostURL(), resp.StatusCode)
	}
	buf := make([]byte, 1<<20)
	n, _ := resp.Body.Read(buf)
	stats := string(buf[:n])
	t.Logf("headroom /stats after the real prompt: %s", stats)

	if strings.Contains(stats, `"api_requests":0`) {
		t.Error(`headroom /stats still shows "api_requests":0 after a real Claude prompt — ` +
			"claude-agent-acp did NOT actually route through Headroom (ANTHROPIC_BASE_URL was " +
			"either ignored, or the process didn't inherit spec.Env the way Connect intends).")
	}
}
