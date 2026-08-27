# opencode sandbox image

No official Docker image exists for opencode (checked 2026-08-26 —
only third-party/community ones, none from the sst/opencode org
itself), so this is a custom image following the same pattern as
`sandbox/claude/`'s adapted official devcontainer: default-deny iptables
firewall, non-root user, firewall setup run by `entrypoint.sh` before
the ACP process is exec'd.

## Build

```sh
docker build -t chorus-opencode-sandbox sandbox/opencode/
```

## Auth / backend

opencode's zero-config free-tier fallback (`providerID=opencode`,
`model=big-pickle` and friends — "OpenCode Zen") works out of the box:
`opencode.ai` is baked into the base firewall allowlist (confirmed live
2026-08-27 — opencode's own docs put the API at
`https://opencode.ai/zen/v1/...`, not a separate `api.opencode.ai`
subdomain as originally guessed; a real sandboxed prompt against it was
verified working end-to-end through chorus). No
`CHORUS_SANDBOX_ALLOW_HOSTS` needed for this case at all.

**This deployment instead points opencode at a self-run Ollama endpoint,
reachable only over the company VPN** — set:

```sh
-e CHORUS_SANDBOX_ALLOW_HOSTS=ollama.internal.company.com:11434
```

(the `:11434` is accepted but ignored — this firewall allowlists by
destination IP, not port; only the hostname is actually resolved and
added).

**This is the hardest, least-guaranteed part of the whole sandboxing
feature — read before assuming it will just work.** The firewall
allowlist above only controls what the *container* is permitted to
reach; it does nothing to guarantee the underlying Rancher Desktop
backend VM (WSL2 on Windows, Lima on macOS/Linux) can actually route to
the VPN in the first place. That depends entirely on your VPN client's
own behavior — full-tunnel vs. split-tunnel, whether it publishes routes
into WSL2's/Lima's virtual network — which chorus has no visibility into
or control over. If a sandboxed opencode can't reach the Ollama
endpoint:

1. First confirm the **host** (outside any container) can reach it while
   connected to VPN — if not, this is a VPN/network problem unrelated to
   sandboxing at all.
2. If the host can reach it but the sandboxed container can't, it's
   likely the backend VM's routing — check Rancher Desktop's network
   mode settings, or try connecting to VPN *before* starting Rancher
   Desktop's daemon.
3. Only once both of those check out is it worth suspecting the firewall
   allowlist itself (e.g. a typo'd hostname, or the endpoint using a
   hostname vs. IP your VPN's DNS doesn't resolve the same way inside
   the VM).

## `agents.yaml` snippet

```yaml
- name: opencode
  spawn: ["docker", "run", "--rm", "-i",
          "--cap-add=NET_ADMIN", "--cap-add=NET_RAW",
          "-v", "{{CWD}}:/workspace",
          "-e", "CHORUS_SANDBOX_ALLOW_HOSTS=ollama.internal.company.com:11434",
          "chorus-opencode-sandbox"]
  workdir: /workspace
  cost_tier: free
```

## Verifying containment

Recommended order, given the VPN dependency above — isolate "does the
sandbox mechanism work at all" from "can it reach the VPN-bound
endpoint" as two separate questions:

1. **First, without VPN**: run against opencode's free-tier backend
   with `CHORUS_SANDBOX_ALLOW_HOSTS` unset (it's baked into the base
   allowlist now), confirm the base mechanism — read/write inside
   `/workspace` works, nothing outside it is reachable, an unrelated
   host (`curl https://example.com`) is blocked, a real prompt completes
   (all confirmed live 2026-08-27, including a real prompt through
   chorus itself).
2. **Then, connected to VPN**: switch to the real Ollama endpoint and
   confirm a real prompt completes against it. If it doesn't, work
   through the three checks above before assuming the image itself is
   broken.
