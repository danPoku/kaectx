// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package redact scrubs secrets out of text before it's written to the
// database. Tool outputs routinely contain API keys (a Bash line that did
// `cat .env`), and a tool call's own input can contain one too (an agent
// writing a config file). Everything that lands in messages.content or
// messages.raw goes through Redact first — no exceptions, since we can't
// know in advance which sessions will have touched a secret.
package redact

import (
	"math"
	"regexp"
)

const placeholder = "[REDACTED]"

// namedPatterns catch secrets with a recognizable shape: a vendor prefix or
// a structural format that's essentially never legitimate prose or code.
// These run first so the generic fallback below doesn't need to duplicate
// them.
var namedPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"aws-access-key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"github-token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,255}`)}, // ghp_, gho_, ghu_, ghs_, ghr_
	{"slack-token", regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,72}`)},
	{"google-api-key", regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`)},
	{"stripe-live-key", regexp.MustCompile(`(sk|rk)_live_[0-9A-Za-z]{16,}`)},
	{"openai-anthropic-key", regexp.MustCompile(`sk-(ant-|proj-)?[A-Za-z0-9_\-]{20,}`)},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`)},
	{"private-key", regexp.MustCompile(`(?is)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{"bearer-token", regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{10,}`)}, // Authorization: Bearer <token>
}

// envLine catches `KEY=value` assignments shaped like a line out of a .env
// file: an uppercase, underscore-separated name at the start of a line. Only
// the value is redacted — the key name itself is usually useful context
// ("this session touched DATABASE_URL") without being a secret.
var envLine = regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*)=(\S+)`)

// highEntropyToken is the fallback for secrets that don't match a known
// vendor prefix. It requires BOTH a letter and a digit in the token, which
// is true of almost every generated credential (base64/hex/base62) but
// false of the vast majority of ordinary identifiers and English prose —
// that's what keeps this from redacting every camelCase function name it
// sees.
var highEntropyToken = regexp.MustCompile(`[A-Za-z0-9+/_\-]{20,}`)

const minEntropyBitsPerChar = 4.3

// Redact replaces likely secrets in s with a fixed placeholder: named
// vendor-token patterns, .env-style KEY=VALUE assignments, and a generic
// high-entropy fallback. It over-redacts on purpose — a false positive
// blanks out a UUID or a git commit hash, a false negative leaks a key — so
// when in doubt, this redacts.
func Redact(s string) string {
	for _, p := range namedPatterns {
		s = p.re.ReplaceAllString(s, placeholder)
	}
	s = envLine.ReplaceAllString(s, "${1}="+placeholder)
	s = highEntropyToken.ReplaceAllStringFunc(s, func(tok string) string {
		if looksGenerated(tok) && shannonEntropy(tok) >= minEntropyBitsPerChar {
			return placeholder
		}
		return tok
	})
	return s
}

// looksGenerated requires a mix of letters and digits, which ordinary
// identifiers and prose rarely have over a 20+ character span but generated
// tokens (base64, hex, base62) almost always do.
func looksGenerated(tok string) bool {
	hasLetter, hasDigit := false, false
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			hasLetter = true
		}
	}
	return hasLetter && hasDigit
}

func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := make(map[rune]int)
	for _, r := range s {
		counts[r]++
	}
	n := float64(len(s))
	entropy := 0.0
	for _, c := range counts {
		p := float64(c) / n
		entropy -= p * math.Log2(p)
	}
	return entropy
}

// Scan reports which redaction rules still match s, without changing it. It
// exists for auditing already-stored text: a hit means something Redact would
// scrub today got into the database anyway (rules added after that row was
// ingested, or a bug). It returns the rule names, never the matched text, so
// an audit report can't itself leak what it found. The result is empty for
// text Redact would leave alone, including text Redact already scrubbed.
func Scan(s string) []string {
	var hits []string
	for _, p := range namedPatterns {
		if p.re.MatchString(s) {
			hits = append(hits, p.name)
		}
	}
	// A placeholder value is what a previous pass left behind, not a leak.
	for _, m := range envLine.FindAllStringSubmatch(s, -1) {
		if m[2] != placeholder {
			hits = append(hits, "env-line")
			break
		}
	}
	for _, tok := range highEntropyToken.FindAllString(s, -1) {
		if looksGenerated(tok) && shannonEntropy(tok) >= minEntropyBitsPerChar {
			hits = append(hits, "high-entropy-token")
			break
		}
	}
	return hits
}
