#!/bin/bash
# Default-deny egress firewall — same mechanism as sandbox/claude/'s and
# sandbox/opencode/'s (adapted from Anthropic's own reference), tailored
# for Gemini CLI's OAuth ("Sign in with Google") + Code Assist auth path.
#
# Base allowlist covers GitHub (git/gh operations), npm's registry
# (installing a project's own dependencies), and the three Google
# endpoints this specific auth path needs: `oauth2.googleapis.com`
# (token refresh), `accounts.google.com` (OAuth), and
# `cloudcode-pa.googleapis.com` (the actual Code Assist API endpoint
# gemini-cli calls for OAuth-authenticated requests — confirmed via
# gemini-cli's own GitHub issues, e.g. #2253/#26036/#26105, NOT
# generativelanguage.googleapis.com, which is the separate direct-API-key
# endpoint this auth path does NOT use). A Vertex AI deployment instead
# needs its regional aiplatform.googleapis.com endpoint via
# CHORUS_SANDBOX_ALLOW_HOSTS — see sandbox/claude/init-firewall.sh's
# comment for that case, which applies here too if you switch Gemini to
# Vertex auth instead of OAuth.
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

add_domain() {
    local domain="$1"
    echo "Resolving $domain..."
    local ips
    ips=$(dig +noall +answer A "$domain" | awk '$4 == "A" {print $5}')
    if [ -z "$ips" ]; then
        echo "ERROR: Failed to resolve $domain"
        exit 1
    fi
    while read -r ip; do
        if [[ ! "$ip" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
            echo "ERROR: Invalid IP from DNS for $domain: $ip"
            exit 1
        fi
        echo "Adding $ip for $domain"
        ipset add allowed-domains "$ip" 2>/dev/null || true
    done < <(echo "$ips")
}

for domain in "registry.npmjs.org" "oauth2.googleapis.com" "accounts.google.com" "cloudcode-pa.googleapis.com"; do
    add_domain "$domain"
done

# Deployment-specific additions (e.g. Vertex AI's regional endpoint, if
# switching this agent to Vertex auth instead of OAuth) — never baked
# into the image itself.
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
