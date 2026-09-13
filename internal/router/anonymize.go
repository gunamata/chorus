package router

import "regexp"

// anonymize redacts high-signal sensitive patterns from text bound for the
// routing-decision prompt (see BuildDecisionPrompt) — emails, IPv4
// addresses, and API-key/token-shaped strings. Deliberately not
// general-purpose PII scrubbing (no names, phone numbers, addresses):
// those need context/NLP to catch reliably and would risk mangling
// ordinary prose in a routing prompt that still needs to read as a
// coherent task description. Order matters — the token pattern is broad
// enough to also match parts of what the email/IP patterns already
// replaced, so it runs last, against text that's already been redacted.
var (
	emailPattern = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	ipv4Pattern  = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
	// tokenPattern covers named prefixes for common providers (OpenAI/
	// Anthropic "sk-", GitHub "ghp_"/"gho_"/"github_pat_", AWS "AKIA",
	// generic "Bearer <token>") plus a catch-all for any other 24+ char
	// run of letters/digits/underscore with no hyphen — hyphens are
	// excluded from the catch-all specifically so an ordinary long
	// hyphenated identifier/slug in a coding prompt ("session-establishment
	// -workflow-config") doesn't get mistaken for a key.
	// ponytail: this still can't tell a real secret from a long git SHA or
	// camelCase identifier (false positive), nor catch a short/low-entropy
	// key the named prefixes above don't cover (false negative) — pattern
	// matching has no real notion of entropy. Upgrade path if this proves
	// too noisy or too permissive in practice: a real entropy check
	// (Shannon entropy over the run) instead of a length threshold.
	tokenPattern = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9\-]{10,}|gh[a-z]_[A-Za-z0-9]{10,}|github_pat_[A-Za-z0-9_]{10,}|AKIA[A-Z0-9]{12,}|Bearer\s+[A-Za-z0-9\-_.]{10,}|[A-Za-z0-9_]{24,})\b`)
)

func anonymize(s string) string {
	s = emailPattern.ReplaceAllString(s, "[redacted-email]")
	s = ipv4Pattern.ReplaceAllString(s, "[redacted-ip]")
	s = tokenPattern.ReplaceAllString(s, "[redacted-token]")
	return s
}
