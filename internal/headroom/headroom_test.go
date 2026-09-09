package headroom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestConfig_EnabledOrDefault(t *testing.T) {
	if (Config{}).EnabledOrDefault() {
		t.Fatal("EnabledOrDefault() = true for zero-value Config, want false (opt-in)")
	}
	on := true
	if !(Config{Enabled: &on}).EnabledOrDefault() {
		t.Fatal("EnabledOrDefault() = false with Enabled=true, want true")
	}
	off := false
	if (Config{Enabled: &off}).EnabledOrDefault() {
		t.Fatal("EnabledOrDefault() = true with Enabled=false, want false")
	}
}

func TestConfig_Defaults(t *testing.T) {
	c := Config{}
	if c.ImageOrDefault() != defaultImage {
		t.Fatalf("ImageOrDefault() = %q, want %q", c.ImageOrDefault(), defaultImage)
	}
	if c.PortOrDefault() != defaultPort {
		t.Fatalf("PortOrDefault() = %d, want %d", c.PortOrDefault(), defaultPort)
	}
	if c.ModeOrDefault() != defaultMode {
		t.Fatalf("ModeOrDefault() = %q, want %q", c.ModeOrDefault(), defaultMode)
	}
}

func TestConfig_Overrides(t *testing.T) {
	c := Config{Image: "custom/image:tag", Port: 9999, Mode: "token"}
	if c.ImageOrDefault() != "custom/image:tag" {
		t.Fatalf("ImageOrDefault() = %q, want the configured override", c.ImageOrDefault())
	}
	if c.PortOrDefault() != 9999 {
		t.Fatalf("PortOrDefault() = %d, want 9999", c.PortOrDefault())
	}
	if c.ModeOrDefault() != "token" {
		t.Fatalf("ModeOrDefault() = %q, want \"token\"", c.ModeOrDefault())
	}
}

func TestConfig_PortOrDefault_NonPositiveFallsBackToDefault(t *testing.T) {
	if got := (Config{Port: 0}).PortOrDefault(); got != defaultPort {
		t.Fatalf("PortOrDefault() with Port=0 = %d, want default %d", got, defaultPort)
	}
	if got := (Config{Port: -1}).PortOrDefault(); got != defaultPort {
		t.Fatalf("PortOrDefault() with Port=-1 = %d, want default %d", got, defaultPort)
	}
}

func TestProxy_HostURLAndSandboxURL(t *testing.T) {
	p := &Proxy{containerName: "chorus-headroom-test", port: 8787}
	if got, want := p.HostURL(), "http://127.0.0.1:8787"; got != want {
		t.Fatalf("HostURL() = %q, want %q", got, want)
	}
	if got, want := p.SandboxURL(), "http://host.docker.internal:8787"; got != want {
		t.Fatalf("SandboxURL() = %q, want %q", got, want)
	}
}

// stubDocker replaces runDocker for the duration of a test, recording
// every invocation's args — same stubbing pattern as internal/tui's
// writeClipboard/readClipboardImage, since these tests must never touch
// a real docker daemon.
func stubDocker(t *testing.T, fn func(args []string) ([]byte, error)) *[][]string {
	t.Helper()
	orig := runDocker
	var calls [][]string
	runDocker = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return fn(args)
	}
	t.Cleanup(func() { runDocker = orig })
	return &calls
}

func TestStart_BuildsExpectedDockerRunArgs(t *testing.T) {
	// A fake HTTP server standing in for the health check — Start's own
	// docker-run call is stubbed to a no-op success, but waitHealthy makes
	// a real HTTP request, so it needs somewhere real to hit. Since Start
	// itself picks the URL from cfg.PortOrDefault(), point the config at
	// this test server's actual port.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	calls := stubDocker(t, func(args []string) ([]byte, error) {
		return nil, nil
	})

	cfg := Config{Port: port, Image: "custom/headroom:tag", Mode: "token"}
	p, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if p.port != port {
		t.Fatalf("p.port = %d, want %d", p.port, port)
	}

	if len(*calls) != 1 {
		t.Fatalf("runDocker called %d times, want exactly 1 (the docker run)", len(*calls))
	}
	callArgs := (*calls)[0]
	got := strings.Join(callArgs, " ")
	for _, want := range []string{
		"run -d --name chorus-headroom-",
		"-p " + strconv.Itoa(port) + ":8787",
		"-e HEADROOM_MODE=token",
		"-e ANTHROPIC_API_KEY",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("docker run args = %q, want it to contain %q", got, want)
		}
	}
	// Regression test for a real bug caught by live testing (2026-09):
	// the image name must be the LAST argument, with no trailing command
	// appended after it. The image's own ENTRYPOINT is already `python3
	// -m headroom.cli proxy` with a complete default CMD — any extra
	// positional args after the image name land as unexpected arguments
	// to that already-complete command and make the container exit
	// immediately on a real docker daemon (a scenario this stubbed test
	// can't itself detect, since runDocker never really runs).
	if got := callArgs[len(callArgs)-1]; got != "custom/headroom:tag" {
		t.Fatalf("last docker run arg = %q, want the image name with nothing appended after it", got)
	}
}

func TestStart_RemovesContainerOnHealthCheckFailure(t *testing.T) {
	// No server listening on this port at all — waitHealthy must time out.
	// Use a short startTimeout override via a cancelled-shortly context
	// instead of waiting the real 30s.
	calls := stubDocker(t, func(args []string) ([]byte, error) {
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled — waitHealthy's ctx.Done() case fires immediately

	cfg := Config{Port: 1} // nothing listens on port 1
	_, err := Start(ctx, cfg)
	if err == nil {
		t.Fatal("Start() with an immediately-cancelled context and nothing listening = nil error, want a health-check failure")
	}

	if len(*calls) != 2 {
		t.Fatalf("runDocker called %d times, want 2 (the failed run's cleanup rm -f in addition to the initial run)", len(*calls))
	}
	if (*calls)[1][0] != "rm" {
		t.Fatalf("second runDocker call = %v, want a cleanup \"rm -f\"", (*calls)[1])
	}
}

func TestStart_NoDockerOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // a PATH with no docker executable on it
	_, err := Start(context.Background(), Config{})
	if err == nil {
		t.Fatal("Start() with no docker on PATH = nil error, want a clear failure")
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Fatalf("error = %q, want it to mention docker", err)
	}
}

func TestStop_RunsDockerRmF(t *testing.T) {
	calls := stubDocker(t, func(args []string) ([]byte, error) { return nil, nil })
	p := &Proxy{containerName: "chorus-headroom-abc123", port: 8787}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("runDocker called %d times, want 1", len(*calls))
	}
	want := []string{"rm", "-f", "chorus-headroom-abc123"}
	got := (*calls)[0]
	if len(got) != len(want) {
		t.Fatalf("Stop() docker args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Stop() docker args = %v, want %v", got, want)
		}
	}
}

// serverPort extracts the numeric port from an httptest.Server's URL.
func serverPort(t *testing.T, url string) int {
	t.Helper()
	idx := strings.LastIndexByte(url, ':')
	if idx < 0 {
		t.Fatalf("couldn't find a port in %q", url)
	}
	port, err := strconv.Atoi(url[idx+1:])
	if err != nil {
		t.Fatalf("non-numeric port in %q: %v", url, err)
	}
	return port
}
