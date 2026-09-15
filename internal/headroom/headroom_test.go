package headroom

import (
	"context"
	"errors"
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

// dockerStub is stubDocker configured to answer `docker inspect` the way
// a container in a given state would — Start() now issues an inspect
// call FIRST to decide whether it needs `run`, `start`, or nothing at
// all, so exercising each of those three paths means controlling what
// inspect reports. Every OTHER subcommand (run/start/rm) just succeeds
// with no output, same as the old always-succeed stub.
func dockerStub(t *testing.T, exists, running bool) *[][]string {
	t.Helper()
	return stubDocker(t, func(args []string) ([]byte, error) {
		if args[0] == "inspect" {
			if !exists {
				return nil, errors.New("Error: No such object: " + args[len(args)-1])
			}
			if running {
				return []byte("true\n"), nil
			}
			return []byte("false\n"), nil
		}
		return nil, nil
	})
}

// healthyServer stands in for the health check — waitHealthy makes a
// real HTTP request regardless of how docker itself is stubbed, so every
// Start() test needs somewhere real to hit; Start picks the URL from
// cfg.PortOrDefault(), so callers point Config at this server's actual port.
func healthyServer(t *testing.T) (port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return serverPort(t, srv.URL)
}

func TestStart_CreatesFreshContainerWhenNoneExists(t *testing.T) {
	port := healthyServer(t)
	calls := dockerStub(t, false, false)

	cfg := Config{Port: port, Image: "custom/headroom:tag", Mode: "token"}
	p, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if p.port != port {
		t.Fatalf("p.port = %d, want %d", p.port, port)
	}

	if len(*calls) != 2 {
		t.Fatalf("runDocker called %d times, want 2 (inspect, then run): %v", len(*calls), *calls)
	}
	if (*calls)[0][0] != "inspect" {
		t.Fatalf("first call = %v, want an inspect", (*calls)[0])
	}
	runArgs := (*calls)[1]
	got := strings.Join(runArgs, " ")
	for _, want := range []string{
		"run -d --name chorus-headroom",
		"--restart unless-stopped",
		"-p " + strconv.Itoa(port) + ":8787",
		"-v chorus-headroom-data:/home/nonroot/.headroom",
		"-e HEADROOM_MODE=token",
		"-e HEADROOM_OUTPUT_SHAPER=1",
		"-e HEADROOM_OUTPUT_HOLDOUT=0.1",
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
	if got := runArgs[len(runArgs)-1]; got != "custom/headroom:tag" {
		t.Fatalf("last docker run arg = %q, want the image name with nothing appended after it", got)
	}
}

func TestStart_TreatsRunNameConflictAsSuccess(t *testing.T) {
	// Simulates losing the create race against a concurrent chorus
	// session: inspect says "not exists" (checked before either session's
	// docker run lands), but by the time THIS run executes, the other
	// session has already created it — docker's real error in that case.
	port := healthyServer(t)
	calls := stubDocker(t, func(args []string) ([]byte, error) {
		switch args[0] {
		case "inspect":
			return nil, errors.New("no such object")
		case "run":
			return []byte(`docker: Error response from daemon: Conflict. The container name "/chorus-headroom" is already in use by container "abc123".`),
				errors.New("exit status 125")
		}
		return nil, nil
	})

	if _, err := Start(context.Background(), Config{Port: port}); err != nil {
		t.Fatalf("Start() error = %v, want the name conflict treated as success (reuse the winner)", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("runDocker called %d times, want 2 (inspect, run): %v", len(*calls), *calls)
	}
}

func TestStart_ReusesAlreadyRunningContainer(t *testing.T) {
	port := healthyServer(t)
	calls := dockerStub(t, true, true)

	if _, err := Start(context.Background(), Config{Port: port}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if len(*calls) != 1 || (*calls)[0][0] != "inspect" {
		t.Fatalf("runDocker calls = %v, want exactly one inspect and nothing else — an already-running container needs no docker run/start", *calls)
	}
}

func TestStart_StartsExistingStoppedContainer(t *testing.T) {
	port := healthyServer(t)
	calls := dockerStub(t, true, false)

	if _, err := Start(context.Background(), Config{Port: port}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if len(*calls) != 2 || (*calls)[1][0] != "start" {
		t.Fatalf("runDocker calls = %v, want inspect then \"docker start\" (not \"run\") for an existing-but-stopped container", *calls)
	}
}

func TestStart_RemovesFreshContainerOnHealthCheckFailure(t *testing.T) {
	calls := dockerStub(t, false, false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled — waitHealthy's ctx.Done() case fires immediately

	_, err := Start(ctx, Config{Port: 1}) // nothing listens on port 1
	if err == nil {
		t.Fatal("Start() with an immediately-cancelled context and nothing listening = nil error, want a health-check failure")
	}
	if len(*calls) != 3 {
		t.Fatalf("runDocker called %d times, want 3 (inspect, run, cleanup rm -f): %v", len(*calls), *calls)
	}
	if (*calls)[2][0] != "rm" {
		t.Fatalf("third runDocker call = %v, want a cleanup \"rm -f\"", (*calls)[2])
	}
}

func TestStart_DoesNotRemoveReusedContainerOnHealthCheckFailure(t *testing.T) {
	// Already running, per inspect — but nothing actually listens on port
	// 1, so the health check still fails. Since Start didn't create this
	// container itself, it must NOT rm -f someone else's container over
	// what could just be a transient timeout.
	calls := dockerStub(t, true, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Start(ctx, Config{Port: 1})
	if err == nil {
		t.Fatal("Start() with nothing listening = nil error, want a health-check failure")
	}
	if len(*calls) != 1 || (*calls)[0][0] != "inspect" {
		t.Fatalf("runDocker calls = %v, want only the inspect — no rm -f on a reused container", *calls)
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
