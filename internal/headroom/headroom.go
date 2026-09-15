// Package headroom optionally runs Headroom
// (https://docs.headroomlabs.ai/docs) — a local compression proxy that
// sits between an agent CLI and its LLM provider, shrinking tool
// outputs/logs/JSON/code before the model sees them — as a single,
// long-lived Docker container shared across chorus runs, not spun up and
// torn down with each one: Start reuses it if it's already running,
// starts it (via `docker start`, not `docker run`) if it exists but is
// stopped, and only `docker run`s a fresh one if it's never existed at
// all. `--restart unless-stopped` (baked in at that first `docker run`)
// makes it survive a Docker engine/Desktop restart on its own; a named
// volume persists its savings/cache data (~/.headroom inside the
// container) across recreation. chorus never stops it — Proxy has no
// caller in main.go's normal run path that tears it down, deliberately,
// so it keeps running (and keeps its provider-side prompt cache warm)
// after chorus exits. Stop still exists for tests/manual cleanup, just
// isn't wired into chorus's own shutdown.
//
// Deliberately containerized for BOTH non-sandboxed and sandboxed agents,
// not just a bare `headroom proxy` host process: a single running
// container works for both, since its published port is reachable as
// http://127.0.0.1:{port} from host-mode agent subprocesses AND as
// http://host.docker.internal:{port} from a SANDBOXED agent's own
// container (see Proxy.SandboxURL's doc comment for the Rancher Desktop
// assumption this rests on, and README/CLAUDE.md for the security
// tradeoff of publishing to every host interface rather than 127.0.0.1
// only, which host.docker.internal needs to actually reach it).
//
// chorus never talks to Headroom's HTTP API directly — a running Proxy
// only ever hands out URLs (HostURL/SandboxURL) for an agent's OWN
// outbound API calls to be redirected at, via that agent's provider-
// specific base-URL env var (e.g. ANTHROPIC_BASE_URL for Claude Code).
// Which env var name a given agent's CLI actually honors is never
// guessed here — same "never guess, let the user configure it once
// they've confirmed the real value" discipline as session.Spec.AutoMode:
// the agents.yaml author writes the mapping explicitly (README has
// worked examples for both modes).
package headroom

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Config is agents.yaml's optional top-level `headroom:` block.
type Config struct {
	Enabled *bool  `yaml:"enabled"`
	Image   string `yaml:"image"`
	Port    int    `yaml:"port"`
	// Mode selects Headroom's own proxy compression mode — "cache"
	// (default: freezes prior turns so a provider's prompt-cache prefix is
	// never busted) or "token" (maximizes per-request compression at the
	// cost of cache stability). Passed straight through as HEADROOM_MODE;
	// chorus doesn't interpret it.
	Mode string `yaml:"mode"`
	// OutputShaper/OutputHoldout tune Headroom's output shaping, passed
	// through as HEADROOM_OUTPUT_SHAPER/HEADROOM_OUTPUT_HOLDOUT. Pointers
	// so an explicit "0" is distinguishable from unset (which defaults).
	OutputShaper  *string `yaml:"output_shaper"`
	OutputHoldout *string `yaml:"output_holdout"`
}

// defaultImage/defaultPort/defaultMode are used when the corresponding
// Config field is unset. defaultPort/defaultMode match Headroom's own
// documented defaults (8787/cache).
//
// defaultImage is deliberately NOT the bare `:latest` tag — Headroom
// publishes an 8-way tag matrix (every combination of the `code`,
// `slim`, and `nonroot` build modifiers; `latest` is the variant with
// none of them). `code` adds the `code` pip extra (Tree-sitter AST
// parsing, CodeCompressor's AST-aware compression path for source code —
// the capability that actually matters for chorus's coding-agent tool
// output). `nonroot` runs as uid 1000 instead of root, at no functional
// cost (Start mounts no volume into this container, so there's no host-
// file ownership mismatch to worry about).
//
// `slim` is DELIBERATELY NOT used, despite being the smaller/more locked-
// down (distroless) option in principle — CONFIRMED LIVE (2026-09, real
// docker run against ghcr.io/headroomlabs-ai/headroom:code-slim-nonroot
// AND :slim-nonroot on Rancher Desktop/Windows, WSL2 linux/amd64 backend):
// every `slim`-tagged variant segfaults on startup (exit 139, zero log
// output — not an OOM kill, not this codebase's code, confirmed by
// testing the non-`code` slim-nonroot variant too, which crashed
// identically, isolating it to the distroless base itself, not the code
// extra). code-nonroot (non-slim) started cleanly and served real
// requests in the same test. This may be specific to this
// environment/backend — re-test `slim` variants before reconsidering,
// don't just flip it back based on the reasoning above alone.
//
// Published multi-arch (amd64+arm64) same as every other variant, so
// this isn't an Apple-Silicon-vs-Intel tradeoff either.
const (
	defaultImage         = "ghcr.io/headroomlabs-ai/headroom:code-nonroot"
	defaultPort          = 8787
	defaultMode          = "cache"
	defaultOutputShaper  = "1"
	defaultOutputHoldout = "0.1"
)

// EnabledOrDefault defaults to false when unset — same opt-in convention
// as policy.Delegation/Compaction: this starts (or reuses) a long-lived
// Docker container, not something to turn on by silent default.
func (c Config) EnabledOrDefault() bool {
	return c.Enabled != nil && *c.Enabled
}

// ImageOrDefault reports the effective container image.
func (c Config) ImageOrDefault() string {
	if c.Image == "" {
		return defaultImage
	}
	return c.Image
}

// PortOrDefault reports the effective host port Headroom's container
// publishes on.
func (c Config) PortOrDefault() int {
	if c.Port <= 0 {
		return defaultPort
	}
	return c.Port
}

// ModeOrDefault reports the effective HEADROOM_MODE value.
func (c Config) ModeOrDefault() string {
	if c.Mode == "" {
		return defaultMode
	}
	return c.Mode
}

// OutputShaperOrDefault reports the effective HEADROOM_OUTPUT_SHAPER value.
func (c Config) OutputShaperOrDefault() string {
	if c.OutputShaper == nil {
		return defaultOutputShaper
	}
	return *c.OutputShaper
}

// OutputHoldoutOrDefault reports the effective HEADROOM_OUTPUT_HOLDOUT value.
func (c Config) OutputHoldoutOrDefault() string {
	if c.OutputHoldout == nil {
		return defaultOutputHoldout
	}
	return *c.OutputHoldout
}

// providerKeyEnvVars are passed to the container as BARE `-e NAME` flags
// (same convention agents.yaml.sandbox.* already uses for
// CHORUS_SANDBOX_ALLOW_HOSTS/GOOGLE_CLOUD_PROJECT — pulls the value from
// chorus's own process environment if set, omits it entirely if not).
// CONFIRMED NOT required for the core proxy-passthrough case (2026-09,
// internal/session/live_headroom_test.go): a real claude-agent-acp
// request through a real Headroom container worked end to end — real
// compression, real reply — with NEITHER ANTHROPIC_API_KEY nor
// OPENAI_API_KEY set anywhere in the test environment. An already-
// authenticated client (Claude Code sending its own subscription auth
// header) has that header forwarded upstream unchanged regardless of
// whether Headroom has its own copy of a provider key. These are still
// passed defensively because Headroom's docs describe using a provider
// key for more accurate token-count-based stats, which wasn't
// specifically exercised by that test.
var providerKeyEnvVars = []string{
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"GOOGLE_APPLICATION_CREDENTIALS",
}

// Proxy is a running, chorus-managed Headroom container.
type Proxy struct {
	containerName string
	port          int
}

// HostURL is the base URL a NON-sandboxed (bare host process) agent
// should redirect its own provider API calls to.
func (p *Proxy) HostURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", p.port)
}

// SandboxURL is the base URL a SANDBOXED agent (running inside its own
// `docker run` container, per agents.yaml.sandbox.*) should redirect its
// own provider API calls to. Rests on Rancher Desktop's host.docker.internal
// support (confirmed by Rancher Desktop's own docs to work with no extra
// --add-host flag — that flag's host-gateway value is explicitly a
// Docker-Desktop-only feature Rancher Desktop doesn't support, so don't
// add it). UNVERIFIED here: whether host.docker.internal reaches a port
// published on every host interface consistently across all three
// platforms Rancher Desktop supports — see Start's doc comment for why
// the container's port is published broadly rather than to 127.0.0.1
// only, and "Known limitations" for the open question this leaves.
func (p *Proxy) SandboxURL() string {
	return fmt.Sprintf("http://host.docker.internal:%d", p.port)
}

// runDocker is a package-level function var (same stubbing pattern as
// internal/tui's writeClipboard/readClipboardImage) so Start/Stop's
// argument-building logic is unit-testable without a real docker daemon.
var runDocker = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	return cmd.CombinedOutput()
}

// startTimeout/healthCheckInterval bound how long Start waits for the
// container to report healthy before giving up — Headroom's own startup
// (Python interpreter +, depending on the image variant, loading local
// ML compression models) is not instant.
const (
	startTimeout        = 30 * time.Second
	healthCheckInterval = 500 * time.Millisecond
)

// containerName/volumeName are fixed, not per-run — the whole point of
// this package's "reuse, don't recreate, never stop" model (package doc
// comment) is a single shared instance, so `docker run --name`/`-v` need
// a stable target to find across chorus invocations, not a fresh random
// one each time.
const (
	containerName = "chorus-headroom"
	volumeName    = "chorus-headroom-data"
)

// inspectState reports whether name exists at all, and if so, whether
// it's currently running — via `docker inspect`, not `docker ps`, since
// inspect still finds a STOPPED container (ps needs -a for that, and
// then distinguishing "stopped" from "never existed" from its output is
// more parsing than `docker inspect`'s explicit "no such object" already
// gives for free). exists is false (not an error) when the container has
// simply never been created.
func inspectState(ctx context.Context, name string) (exists, running bool, err error) {
	out, err := runDocker(ctx, "inspect", "--format", "{{.State.Running}}", name)
	if err != nil {
		return false, false, nil
	}
	return true, strings.TrimSpace(string(out)) == "true", nil
}

// Start ensures Headroom's container is running and waits for it to
// answer health checks before returning:
//   - already running -> reused as-is, no docker command needed.
//   - exists but stopped (e.g. after a host reboot on a Docker Engine
//     without `--restart` support, or a manual `docker stop`) -> `docker
//     start`, which preserves its original --restart policy/volume/port.
//   - never existed -> `docker run` with `--restart unless-stopped` (so a
//     future Docker engine restart brings it back on its own) and a
//     named volume mounted at the image's own state directory (so
//     savings/cache data survives being recreated). Two chorus sessions
//     racing to create it both take this branch; the `docker run` loser
//     sees a name-conflict error, which is treated as success (reuse the
//     winner's container) rather than a failure — see the inline comment
//     at that check.
//
// The container's port is published on every host interface (`-p
// {port}:8787`, not `-p 127.0.0.1:{port}:8787`) — deliberately, not an
// oversight: on Linux, Rancher Desktop's containers reach the host over a
// real docker bridge interface, which does NOT reach a
// 127.0.0.1-only-bound host port (unlike Docker Desktop's macOS/Windows
// VM-proxied networking, which typically does) — so binding to
// loopback-only would silently break SANDBOXED agent reachability
// specifically on Linux. This is a real security tradeoff (the proxy
// becomes reachable from any local process, and depending on Rancher
// Desktop's VM networking possibly the LAN — see README) accepted so one
// running container serves both modes uniformly; not something chorus
// can make purely safe from the Go side alone. On failure of a FRESH
// `docker run` specifically, the partially-started container is removed
// so the next attempt isn't blocked by a broken container occupying
// containerName — a reused (already-existing) container is left alone on
// a health-check failure, since chorus didn't create it this run and
// removing someone's pre-existing container over a possibly-transient
// timeout is too destructive a default.
func Start(ctx context.Context, cfg Config) (*Proxy, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("headroom.enabled is true but no \"docker\" executable was found on PATH: %w", err)
	}
	port := cfg.PortOrDefault()
	p := &Proxy{containerName: containerName, port: port}

	exists, running, err := inspectState(ctx, containerName)
	if err != nil {
		return nil, fmt.Errorf("inspect headroom container: %w", err)
	}

	freshlyCreated := false
	switch {
	case running:
		// Nothing to do — reuse it as-is.
	case exists:
		if out, err := runDocker(ctx, "start", containerName); err != nil {
			return nil, fmt.Errorf("docker start (headroom): %w: %s", err, out)
		}
	default:
		freshlyCreated = true
		args := []string{"run", "-d", "--name", containerName,
			"--restart", "unless-stopped",
			"-p", fmt.Sprintf("%d:8787", port),
			"-v", volumeName + ":/home/nonroot/.headroom",
			"-e", "HEADROOM_HOST=0.0.0.0",
			"-e", "HEADROOM_PORT=8787",
			"-e", "HEADROOM_MODE=" + cfg.ModeOrDefault(),
			"-e", "HEADROOM_OUTPUT_SHAPER=" + cfg.OutputShaperOrDefault(),
			"-e", "HEADROOM_OUTPUT_HOLDOUT=" + cfg.OutputHoldoutOrDefault(),
		}
		for _, v := range providerKeyEnvVars {
			args = append(args, "-e", v)
		}
		// No trailing command: the image's own ENTRYPOINT is already
		// `python3 -m headroom.cli proxy` with a default CMD of `--host
		// 0.0.0.0 --port 8787` — CONFIRMED LIVE (2026-09) that appending
		// an explicit "headroom proxy --host ... --port ..." command
		// here (an earlier version of this code did) breaks startup
		// outright: the image's own CLI parser sees that as extra
		// positional arguments after its own already-complete default
		// CMD and exits immediately ("Error: Got unexpected extra
		// arguments (headroom proxy)"). The -e HEADROOM_HOST/
		// HEADROOM_PORT above are enough on their own.
		args = append(args, cfg.ImageOrDefault())

		if out, err := runDocker(ctx, args...); err != nil {
			// Two chorus sessions starting within the same window both
			// see "not running" and both try to create it — CONFIRMED
			// LIVE (2026-09, two real concurrent `docker run`s) that the
			// loser gets back "Conflict... already in use by container
			// ...", not a hang or a corrupted state. Docker's own --name
			// uniqueness is already the lock; the loser just needs to
			// stop treating this as an error and reuse what the winner
			// created, same as the "exists but stopped" path above.
			if !strings.Contains(string(out), "already in use") {
				return nil, fmt.Errorf("docker run (headroom): %w: %s", err, out)
			}
			freshlyCreated = false
		}
	}

	if err := waitHealthy(ctx, p.HostURL()); err != nil {
		if freshlyCreated {
			_, _ = runDocker(context.Background(), "rm", "-f", containerName)
		}
		return nil, fmt.Errorf("headroom container never became healthy: %w", err)
	}
	return p, nil
}

// waitHealthy polls baseURL until it answers or startTimeout elapses.
// Headroom doesn't document a dedicated /health(z) endpoint; /stats
// (mentioned in their quickstart as the monitoring endpoint) is used
// here — any HTTP response at all (regardless of status code) is treated
// as "the proxy is up," since the only failure mode being guarded
// against is "nothing is listening yet."
func waitHealthy(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(startTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/stats", nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				return nil
			}
			lastErr = err
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(healthCheckInterval):
		}
	}
	return fmt.Errorf("timed out after %s: %w", startTimeout, lastErr)
}

// stopTimeout bounds Stop's own docker call — deliberately not tied to a
// caller's ctx, which may already be cancelled by the time cleanup runs.
const stopTimeout = 10 * time.Second

// Stop removes the container. NOT called anywhere in chorus's own normal
// run path (package doc comment: Headroom is meant to keep running after
// chorus exits) — exists for tests and manual cleanup only. Safe to call
// on a Proxy whose container already exited on its own (`docker rm -f`
// on an already-stopped container is not an error).
func (p *Proxy) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if out, err := runDocker(ctx, "rm", "-f", p.containerName); err != nil {
		return fmt.Errorf("docker rm -f %s: %w: %s", p.containerName, err, out)
	}
	return nil
}
