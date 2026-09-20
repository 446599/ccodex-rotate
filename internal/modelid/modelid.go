// Package modelid normalizes a requested model identifier into a stable
// canonical name. It is intentionally small: ccodex-rotate only needs to agree
// on one name per model so turn-state caching, probing and injection all key
// off the same value (e.g. "gpt-5.4" and "gpt-5.4-high" must collapse).
package modelid

import (
	"regexp"
	"strings"
)

// Resolver canonicalizes model ids, optionally with user overrides.
type Resolver struct {
	aliases map[string]string
}

// New returns a Resolver. extra (public -> canonical) overrides the built-ins.
func New(extra map[string]string) *Resolver {
	r := &Resolver{aliases: map[string]string{}}
	for k, v := range builtinAliases {
		r.aliases[k] = v
	}
	for k, v := range extra {
		k = clean(k)
		if k == "" {
			continue
		}
		r.aliases[k] = clean(v)
	}
	return r
}

// Canonical normalizes raw using the built-in rules only.
func Canonical(raw string) string { return New(nil).Canonical(raw) }

// Canonical normalizes a requested model id.
func (r *Resolver) Canonical(raw string) string {
	id := clean(raw)
	if id == "" {
		return ""
	}
	id = stripReasoningSuffix(id)
	id = stripDateSuffix(id)
	if mapped, ok := r.aliases[id]; ok && mapped != "" {
		return mapped
	}
	// Longest prefixes first so gpt-5.3-codex-spark wins over gpt-5.3-codex.
	for _, p := range versionPrefixes {
		if id == p {
			return p
		}
	}
	for _, p := range versionPrefixes {
		if strings.HasPrefix(id, p+"-") {
			rest := id[len(p)+1:]
			if isKnownSuffix(rest) {
				return p
			}
		}
	}
	return id
}

// clean trims, keeps the last path segment, lowercases and hyphenates spaces.
func clean(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ToLower(s)
	s = strings.Join(strings.Fields(s), "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}

func stripReasoningSuffix(id string) string {
	for _, suf := range reasoningSuffixes {
		if strings.HasSuffix(id, "-"+suf) {
			return strings.TrimSuffix(id, "-"+suf)
		}
	}
	return id
}

var dateSuffix = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)

func stripDateSuffix(id string) string { return dateSuffix.ReplaceAllString(id, "") }

func isKnownSuffix(s string) bool {
	if s == "" {
		return false
	}
	for _, suf := range reasoningSuffixes {
		if s == suf {
			return true
		}
	}
	return dateSuffix.MatchString("-" + s)
}

var reasoningSuffixes = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// versionPrefixes are canonical Codex-era model families, longest first.
var versionPrefixes = []string{
	"gpt-6-astra",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.3-codex-spark",
	"gpt-5.3-codex",
	"gpt-5.4-mini",
	"gpt-5.4-nano",
	"gpt-5.5-pro",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.2",
}

// builtinAliases maps legacy/suffixed names onto their canonical family.
var builtinAliases = map[string]string{
	"gpt-5.3":             "gpt-5.3-codex",
	"gpt-5-codex":         "gpt-5.3-codex",
	"gpt-5.1-codex":       "gpt-5.3-codex",
	"gpt-5.1-codex-max":   "gpt-5.3-codex",
	"gpt-5.1-codex-mini":  "gpt-5.3-codex",
	"codex-mini-latest":   "gpt-5.3-codex",
	"gpt-5.2-codex":       "gpt-5.2",
	"gpt-5":               "gpt-5.4",
	"gpt-5-mini":          "gpt-5.4",
	"gpt-5-nano":          "gpt-5.4",
	"gpt-5.1":             "gpt-5.4",
	"gpt-5.4-chat-latest": "gpt-5.4",
	"codex-auto-review":   "codex-auto-review",
}
