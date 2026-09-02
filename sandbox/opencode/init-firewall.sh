#!/bin/bash
# Default-deny egress firewall — same mechanism as sandbox/claude/'s
# (adapted from Anthropic's own reference), tailored for opencode.
#
# Base allowlist covers GitHub (git/gh operations), npm's registry
# (installing a project's own dependencies), and opencode.ai — opencode's
# own "Zen" free-tier backend (confirmed live 2026-08-27, correcting an
# earlier "we don't know the exact domain" placeholder: opencode's own
# docs put the API at https://opencode.ai/zen/v1/..., not a separate
# api.opencode.ai subdomain as originally guessed). This covers the
# common case (opencode's public free tier, zero configured credentials)
# out of the box. A deployment pointing opencode at something else
# instead — this project's own self-run Ollama endpoint, reachable only
# over the company VPN — still needs CHORUS_SANDBOX_ALLOW_HOSTS set
# (comma-separated; an optional ":port" suffix is accepted and ignored —
# this firewall allowlists by destination IP, not port).
#
# Note: this only controls what the CONTAINER is allowed to reach. If
# the backend is VPN-bound, Rancher Desktop's own backend VM (WSL2 on
# Windows, Lima on macOS/Linux) also needs a network path to that VPN —
# a separate, host-level question this script can't answer. See
# sandbox/opencode/README.md.
set -euo pipefail
IFS=$'\n\t'

DOCKER_DNS_RULES=$(iptables-save -t nat | grep "127\.0\.0\.11" || true)

iptables -F
iptables -X
iptables -t nat -F
iptables -t nat -X
iptables -t mangle -F
iptables -t mangle -X
ipset destroy allowed-domains 2>/dev/null || true

if [ -n "$DOCKER_DNS_RULES" ]; then
    echo "Restoring Docker DNS rules..."
    iptables -t nat -N DOCKER_OUTPUT 2>/dev/null || true
    iptables -t nat -N DOCKER_POSTROUTING 2>/dev/null || true
    echo "$DOCKER_DNS_RULES" | xargs -L 1 iptables -t nat
else
    echo "No Docker DNS rules to restore"
fi

iptables -A OUTPUT -p udp --dport 53 -j ACCEPT
iptables -A INPUT -p udp --sport 53 -j ACCEPT
iptables -A INPUT -i lo -j ACCEPT
iptables -A OUTPUT -o lo -j ACCEPT

ipset create allowed-domains hash:net

echo "Fetching GitHub IP ranges..."
gh_ranges=$(curl -s https://api.github.com/meta)
if [ -z "$gh_ranges" ]; then
    echo "ERROR: Failed to fetch GitHub IP ranges"
    exit 1
fi
if ! echo "$gh_ranges" | jq -e '.web and .api and .git' >/dev/null; then
    echo "ERROR: GitHub API response missing required fields"
    exit 1
fi
echo "Processing GitHub IPs..."
while read -r cidr; do
    if [[ ! "$cidr" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}/[0-9]{1,2}$ ]]; then
        echo "ERROR: Invalid CIDR range from GitHub meta: $cidr"
        exit 1
    fi
    echo "Adding GitHub range $cidr"
    ipset add allowed-domains "$cidr"
done < <(echo "$gh_ranges" | jq -r '(.web + .api + .git)[]' | aggregate -q)

# add_domain DOMAIN [STRICT]
# Resolves DOMAIN and adds its A records to the allowlist. STRICT (base
# allowlist): any failure aborts. Non-strict (user-supplied hosts): a host
# that fails to resolve — e.g. a VPN-only Ollama endpoint while the VPN
# isn't connected — is a warning, and the container keeps starting (see
# sandbox/opencode/README.md on the containment-verification caveat).
# Google serves its API endpoints (aiplatform / oauth2 / accounts /
# cloudcode-pa .googleapis.com, ...) from a large, per-query-rotating
# frontend IP pool, so the one-shot A-record snapshot add_domain takes
# below routinely misses the IP the agent resolves at request time — an
# intermittent default-deny drop that surfaces as "API Error: Unable to
# connect". add_google_ranges adds Google's own published netblocks
# (goog.json) instead, the same approach already used for GitHub above,
# covering the whole pool. Fetched at most once, lazily, the first time any
# Google host is added — which for opencode's default (opencode.ai) and
# Ollama backends is never, so this stays a no-op here unless a Google host
# is put in CHORUS_SANDBOX_ALLOW_HOSTS. Purely additive and best-effort: a
# fetch failure warns and leaves the per-host snapshot in place rather than
# aborting the container (same "don't crash on a transient" spirit as the
# non-strict host path) — never worse than not having this at all.
GOOGLE_RANGES_ADDED=""
add_google_ranges() {
    [ -n "$GOOGLE_RANGES_ADDED" ] && return 0
    GOOGLE_RANGES_ADDED=1
    echo "Fetching Google IP ranges (goog.json)..."
    local goog
    goog=$(curl -s https://www.gstatic.com/ipranges/goog.json || true)
    if [ -z "$goog" ] || ! echo "$goog" | jq -e '.prefixes' >/dev/null 2>&1; then
        echo "WARNING: Failed to fetch Google IP ranges — falling back to per-host DNS snapshot only"
        return 0
    fi
    echo "Processing Google IPs..."
    while read -r cidr; do
        [[ "$cidr" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}/[0-9]{1,2}$ ]] || continue
        ipset add allowed-domains "$cidr" 2>/dev/null || true
    done < <(echo "$goog" | jq -r '.prefixes[].ipv4Prefix // empty' | aggregate -q)
    echo "Google IP ranges added"
}

add_domain() {
    local domain="$1"
    local strict="${2:-}"
    local ips
    # A Google-backed host (Vertex, OAuth, Code Assist) needs Google's full
    # published ranges, not just this one snapshot — see add_google_ranges.
    case "$domain" in
        *.googleapis.com|*.google.com|googleapis.com|google.com)
            add_google_ranges ;;
    esac
    echo "Resolving $domain..."
    ips=$(dig +noall +answer A "$domain" | awk '$4 == "A" {print $5}')
    if [ -z "$ips" ]; then
        if [ "$strict" = "strict" ]; then
            echo "ERROR: Failed to resolve $domain"
            exit 1
        fi
        echo "WARNING: Failed to resolve $domain — skipping (it will become allowed once it resolves, e.g. once the VPN is up)"
        return 0
    fi
    local bad_ips=""
    while read -r ip; do
        if [[ ! "$ip" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
            if [ "$strict" = "strict" ]; then
                echo "ERROR: Invalid IP from DNS for $domain: $ip"
                exit 1
            fi
            bad_ips="$bad_ips $ip"
            continue
        fi
        echo "Adding $ip for $domain"
        ipset add allowed-domains "$ip" 2>/dev/null || true
    done < <(echo "$ips")
    if [ -n "$bad_ips" ]; then
        if [ "$strict" = "strict" ]; then
            echo "ERROR: Invalid IP from DNS for $domain:$bad_ips"
            exit 1
        fi
        echo "WARNING: Skipping invalid IP for $domain:$bad_ips"
    fi
}

for domain in "registry.npmjs.org" "opencode.ai"; do
    add_domain "$domain" strict
done

# Deployment-specific: a self-run Ollama endpoint (or any other
# non-default backend) — never baked into the image itself.
if [ -n "${CHORUS_SANDBOX_ALLOW_HOSTS:-}" ]; then
    IFS=',' read -ra extra_hosts <<< "$CHORUS_SANDBOX_ALLOW_HOSTS"
    for host in "${extra_hosts[@]}"; do
        host="${host%%:*}"
        host="$(echo -n "$host" | xargs)"
        [ -z "$host" ] && continue
        add_domain "$host"
    done
fi

HOST_IP=$(ip route | grep default | cut -d" " -f3)
if [ -z "$HOST_IP" ]; then
    echo "ERROR: Failed to detect host IP"
    exit 1
fi
HOST_NETWORK=$(echo "$HOST_IP" | sed "s/\.[0-9]*$/.0\/24/")
echo "Host network detected as: $HOST_NETWORK"
iptables -A INPUT -s "$HOST_NETWORK" -j ACCEPT
iptables -A OUTPUT -d "$HOST_NETWORK" -j ACCEPT

iptables -P INPUT DROP
iptables -P FORWARD DROP
iptables -P OUTPUT DROP

iptables -A INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT
iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT
iptables -A OUTPUT -m set --match-set allowed-domains dst -j ACCEPT
iptables -A OUTPUT -j REJECT --reject-with icmp-admin-prohibited

echo "Firewall configuration complete"
echo "Verifying firewall rules..."
if curl --connect-timeout 5 https://example.com >/dev/null 2>&1; then
    echo "ERROR: Firewall verification failed - was able to reach https://example.com"
    exit 1
else
    echo "Firewall verification passed - unable to reach https://example.com as expected"
fi
if ! curl --connect-timeout 5 https://api.github.com/zen >/dev/null 2>&1; then
    echo "ERROR: Firewall verification failed - unable to reach https://api.github.com"
    exit 1
else
    echo "Firewall verification passed - able to reach https://api.github.com as expected"
fi
