// Package headroom optionally runs Headroom
// (https://docs.headroomlabs.ai/docs) — a local compression proxy that
// sits between an agent CLI and its LLM provider, shrinking tool
// outputs/logs/JSON/code before the model sees them — as a chorus-managed
// Docker container, for the lifetime of one chorus run.
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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os/exec"
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
	defaultImage = "ghcr.io/headroomlabs-ai/headroom:code-nonroot"
	defaultPort  = 8787
	defaultMode  = "cache"
)

// EnabledOrDefault defaults to false when unset — same opt-in convention
// as policy.Delegation/Compaction: this starts an extra Docker container
// for the lifetime of every chorus run, not something to turn on by
// silent default.
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

// newContainerName generates a unique per-run container name
// (chorus-headroom-<8 random hex chars>) — random rather than fixed, so
// two concurrent chorus runs (different projects/terminals) each get
// their own container instead of colliding on `docker run --name`, and
// so a container left behind by an unclean shutdown doesn't block the
// next run from starting a fresh one under the same name.
func newContainerName() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "chorus-headroom-" + hex.EncodeToString(b), nil
}

// Start runs Headroom's container and waits for it to answer health
// checks before returning. The container's port is published on every
// host interface (`-p {port}:8787`, not `-p 127.0.0.1:{port}:8787`) —
// deliberately, not an oversight: on Linux, Rancher Desktop's containers
// reach the host over a real docker bridge interface, which does NOT
// reach a 127.0.0.1-only-bound host port (unlike Docker Desktop's
// macOS/Windows VM-proxied networking, which typically does) — so
// binding to loopback-only would silently break SANDBOXED agent
// reachability specifically on Linux. This is a real security tradeoff
// (the proxy becomes reachable from any local process, and depending on
// Rancher Desktop's VM networking possibly the LAN — see README) accepted
// so one running container serves both modes uniformly; not something
// chorus can make purely safe from the Go side alone. On failure, any
// partially-started container is removed before returning the error.
func Start(ctx context.Context, cfg Config) (*Proxy, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("headroom.enabled is true but no \"docker\" executable was found on PATH: %w", err)
	}
	name, err := newContainerName()
	if err != nil {
		return nil, fmt.Errorf("generate headroom container name: %w", err)
	}
	port := cfg.PortOrDefault()

	args := []string{"run", "-d", "--name", name,
		"-p", fmt.Sprintf("%d:8787", port),
		"-e", "HEADROOM_HOST=0.0.0.0",
		"-e", "HEADROOM_PORT=8787",
		"-e", "HEADROOM_MODE=" + cfg.ModeOrDefault(),
	}
	for _, v := range providerKeyEnvVars {
		args = append(args, "-e", v)
	}
	// No trailing command: the image's own ENTRYPOINT is already
	// `python3 -m headroom.cli proxy` with a default CMD of `--host
	// 0.0.0.0 --port 8787` — CONFIRMED LIVE (2026-09) that appending an
	// explicit "headroom proxy --host ... --port ..." command here (an
	// earlier version of this code did) breaks startup outright: the
	// image's own CLI parser sees that as extra positional arguments
	// after its own already-complete default CMD and exits immediately
	// ("Error: Got unexpected extra arguments (headroom proxy)"). The -e
	// HEADROOM_HOST/HEADROOM_PORT above are enough on their own.
	args = append(args, cfg.ImageOrDefault())

	if out, err := runDocker(ctx, args...); err != nil {
		return nil, fmt.Errorf("docker run (headroom): %w: %s", err, out)
	}

	p := &Proxy{containerName: name, port: port}
	if err := waitHealthy(ctx, p.HostURL()); err != nil {
		_, _ = runDocker(context.Background(), "rm", "-f", name)
		return nil, fmt.Errorf("headroom container started but never became healthy: %w", err)
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

// stopTimeout bounds Stop's own docker call — deliberately NOT tied to
// the caller's ctx: Stop is expected to run from a deferred cleanup path
// during shutdown, when the run's own context may already be cancelled
// (e.g. Ctrl+C), and cleanup should still get a real chance to run rather
// than being aborted by the same cancellation that triggered it.
const stopTimeout = 10 * time.Second

// Stop removes the container. Safe to call on a Proxy whose container
// already exited on its own (`docker rm -f` on an already-stopped
// container is not an error).
func (p *Proxy) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if out, err := runDocker(ctx, "rm", "-f", p.containerName); err != nil {
		return fmt.Errorf("docker rm -f %s: %w: %s", p.containerName, err, out)
	}
	return nil
}
