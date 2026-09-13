package router

import (
	"strings"
	"testing"
)

func TestAnonymize(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantGone string // substring that must NOT survive
		wantHas  string // substring that must appear in its place
	}{
		{"email", "contact me at jane.doe@example.com please", "jane.doe@example.com", "[redacted-email]"},
		{"ipv4", "the host is 10.0.0.42 on the vpn", "10.0.0.42", "[redacted-ip]"},
		{"anthropic key", "use sk-ant-api03-abcdefghijklmnop for auth", "sk-ant-api03-abcdefghijklmnop", "[redacted-token]"},
		{"github token", "token is ghp_1234567890abcdefABCDEF", "ghp_1234567890abcdefABCDEF", "[redacted-token]"},
		{"aws key", "AKIAABCDEFGHIJKLMNOP is the access key", "AKIAABCDEFGHIJKLMNOP", "[redacted-token]"},
		{"bearer token", "Authorization: Bearer abcdEFGH12345678", "Bearer abcdEFGH12345678", "[redacted-token]"},
		{"generic long token", "secret is aZ9bY8cX7dW6eV5fU4gT3hS2", "aZ9bY8cX7dW6eV5fU4gT3hS2", "[redacted-token]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := anonymize(c.in)
			if strings.Contains(got, c.wantGone) {
				t.Fatalf("anonymize(%q) = %q, still contains %q", c.in, got, c.wantGone)
			}
			if !strings.Contains(got, c.wantHas) {
				t.Fatalf("anonymize(%q) = %q, want it to contain %q", c.in, got, c.wantHas)
			}
		})
	}
}

func TestAnonymize_LeavesOrdinaryProseAlone(t *testing.T) {
	in := "please refactor the internal/session package and fix the bug in Connect"
	if got := anonymize(in); got != in {
		t.Fatalf("anonymize(%q) = %q, want it unchanged", in, got)
	}
}

func TestAnonymize_LeavesHyphenatedIdentifiersAlone(t *testing.T) {
	in := "rename the session-establishment-workflow-config field"
	if got := anonymize(in); got != in {
		t.Fatalf("anonymize(%q) = %q, want hyphenated identifiers left alone", in, got)
	}
}
