// Package router implements §9's auto-routing: deciding which agent
// handles a prompt that has no explicit "<agent>: " prefix.
//
// Router.choose(task_text) -> agent_name is deliberately a small, swappable
// strategy (§9): v1 is lowercase keyword matching against policy.yaml's
// rules, first hit wins. This intentionally is NOT model-based — running
// a classifier call would itself burn tokens on a metered agent, defeating
// the point of routing cheap tasks away from it.
package router

import (
	"regexp"

	"chorus/internal/policy"
)

// Decision is the router's answer for one piece of prompt text.
type Decision struct {
	Agent string
	// Matched is true if an explicit rule fired. False means Agent came
	// from Routing.Default (or is empty, if there's no default either) —
	// callers use this to decide whether to honor policy.yaml's
	// ask_when_ambiguous instead of silently guessing.
	Matched bool
}

// Choose picks an agent for text per routing's rules, falling back to
// routing.Default if nothing matches.
func Choose(routing policy.Routing, text string) Decision {
	for _, rule := range routing.Rules {
		for _, kw := range rule.Match {
			if kw == "" {
				continue
			}
			if matchesWord(text, kw) {
				return Decision{Agent: rule.Agent, Matched: true}
			}
		}
	}
	return Decision{Agent: routing.Default, Matched: false}
}

// matchesWord reports whether kw appears in text as a whole word (or
// phrase), case-insensitively — not merely as a substring.
//
// v1 originally used strings.Contains, which meant "fix" matched inside
// "prefix"/"suffix"/"fixture" and "list" matched inside
// "checklist"/"whitelist": real misrouting on ordinary prose, found via
// dedicated test coverage rather than live use. \b works per word-boundary
// position, so it works for multi-word phrases too, not just single
// keywords.
func matchesWord(text, kw string) bool {
	re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(kw) + `\b`)
	if err != nil {
		// kw came from policy.yaml, not user input — but never let a
		// malformed keyword panic or crash routing; just don't match it.
		return false
	}
	return re.MatchString(text)
}
