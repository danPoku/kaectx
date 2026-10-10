// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactNamedPatterns(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"aws access key", "aws_access_key_id = AKIAIOSFODNN7EXAMPLE"},
		{"github token", "export GITHUB_TOKEN=ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"slack bot token", "SLACK_BOT_TOKEN=xoxb-1234567890-abcdefghijklmnop"},
		{"google api key", "key: AIzaSyAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"stripe live key", "sk_live_51H8xyzABCDEFGHIJKLMNOPQR"},
		{"anthropic key", "ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"},
		{"jwt", "Authorization: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dQw4w9WgXcQ_abc123DEF"},
		{"pem private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234567890abcdef\n-----END RSA PRIVATE KEY-----"},
		{"bearer header", "curl -H \"Authorization: Bearer abcdEFGH1234567890tokenvalue\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Redact(c.input)
			if strings.Contains(got, placeholder) == false {
				t.Errorf("Redact(%q) = %q, want it to contain %q", c.input, got, placeholder)
			}
		})
	}
}

func TestRedactEnvLine(t *testing.T) {
	input := "DATABASE_URL=postgres://user:hunter2@localhost/db\nnormal line here"
	got := Redact(input)
	if strings.Contains(got, "hunter2") {
		t.Errorf("Redact(%q) = %q, leaked the env value", input, got)
	}
	if !strings.HasPrefix(got, "DATABASE_URL=[REDACTED]") {
		t.Errorf("Redact(%q) = %q, want the key name preserved", input, got)
	}
	if !strings.Contains(got, "normal line here") {
		t.Errorf("Redact(%q) = %q, want the unrelated line untouched", input, got)
	}
}

func TestRedactHighEntropyFallback(t *testing.T) {
	// A plausible-looking generated token with no known vendor prefix.
	input := "token=Zt8pQ2mK9xL4nR7vB3wJ6yH1cF5dS0aE"
	got := Redact(input)
	if !strings.Contains(got, placeholder) {
		t.Errorf("Redact(%q) = %q, want the high-entropy token redacted", input, got)
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	cases := []string{
		"Let's refactor the ResolveProject function to use a prefix match.",
		"func TestMigrateCreatesCoreSchema(t *testing.T) { ... }",
		"the quick brown fox jumps over the lazy dog, twice, for good measure",
		"github.com/mattn/go-sqlite3",
		"internal/ingest/claudecode/adapter.go",
	}
	for _, s := range cases {
		if got := Redact(s); got != s {
			t.Errorf("Redact(%q) = %q, want it unchanged (false positive)", s, got)
		}
	}
}

func TestRedactIsIdempotent(t *testing.T) {
	input := "ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	once := Redact(input)
	twice := Redact(once)
	if once != twice {
		t.Errorf("Redact is not idempotent: once=%q twice=%q", once, twice)
	}
}

// TestRedactPreservesJSONValidity matters because messages.raw is CHECKed
// with json_valid(raw): redaction runs on the serialized JSON string, and a
// match that straddled a quote or escape would corrupt it.
func TestRedactPreservesJSONValidity(t *testing.T) {
	raw := `{"type":"tool_result","content":"aws_secret=AKIAIOSFODNN7EXAMPLE ok","tool_use_id":"toolu_01UR534WBTLBbAogPQHCRZJj"}`
	got := Redact(raw)
	var v any
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("Redact produced invalid JSON: %v\ngot: %s", err, got)
	}
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("Redact(%q) = %q, left the AWS key in place", raw, got)
	}
}

// Scan must agree with Redact: it flags exactly the text Redact would change,
// and text that has already been through Redact comes back clean.
func TestScanAgreesWithRedact(t *testing.T) {
	leaks := map[string]string{
		"github-token":       "token ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA here",
		"aws-access-key":     "AKIAIOSFODNN7EXAMPLE",
		"env-line":           "DATABASE_URL=postgres://user:hunter2@localhost/db",
		"high-entropy-token": "value a8Fk29xQpLmZ0vR7tYw3NcB5hJd1 end",
	}
	for want, input := range leaks {
		hits := Scan(input)
		found := false
		for _, h := range hits {
			if h == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Scan(%q) = %v, want it to include %q", input, hits, want)
		}
		if again := Scan(Redact(input)); len(again) != 0 {
			t.Errorf("Scan(Redact(%q)) = %v, want no hits", input, again)
		}
	}
	for _, clean := range []string{"just ordinary prose about refactoring", "func handleRequest(w http.ResponseWriter)", "API_KEY=[REDACTED]"} {
		if hits := Scan(clean); len(hits) != 0 {
			t.Errorf("Scan(%q) = %v, want no hits", clean, hits)
		}
	}
}
