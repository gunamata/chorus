//go:build headroom_live

// This file requires a real, reachable Docker daemon and network access to
// pull ghcr.io/headroomlabs-ai/headroom — excluded from the normal `go
// test ./...` run (which must work with no daemon at all, same reason
// internal/session/internal/delegate stay untested by plain `go test` —
// see CLAUDE.md's Test coverage section) via the headroom_live build tag.
// Run explicitly: go test ./internal/headroom/... -tags headroom_live -v
package headroom

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestLive_StartAndStop exercises the REAL Start/Stop code path (not the
// stubbed runDocker used by the rest of this package's tests) against a
// real docker daemon and the real published image — the only way to catch
// a bug like the one live testing found here (2026-09): an earlier
// version appended an explicit "headroom proxy --host ... --port ..."
// command that collided with the image's own complete default CMD and
// made the container exit immediately, which no stubbed-docker unit test
// could ever have caught.
func TestLive_StartAndStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p, err := Start(ctx, Config{})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		if err := p.Stop(); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	}()

	resp, err := http.Get(p.HostURL() + "/stats")
	if err != nil {
		t.Fatalf("GET %s/stats: %v", p.HostURL(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/stats status = %d, want 200", p.HostURL(), resp.StatusCode)
	}
}
