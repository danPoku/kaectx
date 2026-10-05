// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/ingest/codex"
)

// A Codex rollout's first line is session_meta, which creates the session
// row before any prompt has been seen, and the first role=user messages are
// injected environment context. The title must still come out as the
// first thing the person actually typed.
func TestCodexSessionIsTitledFromFirstTypedPrompt(t *testing.T) {
	db := openTestDB(t)
	path := copyFixture(t, "codex/testdata/session.jsonl")
	if err := ingest.Run(db, codex.New(), path); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var title string
	if err := db.QueryRow(`SELECT COALESCE(title, '') FROM sessions WHERE agent = 'codex'`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "How many files are in this directory?" {
		t.Errorf("title = %q, want the first typed prompt", title)
	}
}

func TestSessionTitles(t *testing.T) {
	const key = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	cases := []struct {
		name  string
		lines []string
		want  string
		// absent, when set, replaces want: the title must be non-empty and
		// must not contain it.
		absent string
	}{
		{
			name: "secret in the first prompt never reaches the title",
			lines: []string{
				`{"type":"user","message":{"role":"user","content":"use ANTHROPIC_API_KEY=` + key + ` for this"},"sessionId":"s1","cwd":"/p","timestamp":"2026-09-20T10:00:00Z"}`,
			},
			absent: key,
		},
		{
			name: "injected notification first: title waits for the typed prompt",
			lines: []string{
				`{"type":"user","message":{"role":"user","content":"<task-notification>\n<summary>done</summary>\n</task-notification>"},"sessionId":"s1","cwd":"/p","timestamp":"2026-09-20T10:00:00Z"}`,
				`{"type":"user","message":{"role":"user","content":"rebuild the\n  vendor page"},"sessionId":"s1","cwd":"/p","timestamp":"2026-09-20T10:00:01Z"}`,
			},
			want: "rebuild the vendor page",
		},
		{
			name: "ai-title overrides the fallback when it arrives later",
			lines: []string{
				`{"type":"user","message":{"role":"user","content":"rebuild the vendor page"},"sessionId":"s1","cwd":"/p","timestamp":"2026-09-20T10:00:00Z"}`,
				`{"type":"ai-title","aiTitle":"Vendor page rebuild","sessionId":"s1"}`,
			},
			want: "Vendor page rebuild",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := openTestDB(t)
			path := filepath.Join(t.TempDir(), "s.jsonl")
			if err := os.WriteFile(path, []byte(strings.Join(c.lines, "\n")+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := ingest.Run(db, claudecode.New(), path); err != nil {
				t.Fatalf("Run: %v", err)
			}
			var title string
			if err := db.QueryRow(`SELECT COALESCE(title, '') FROM sessions`).Scan(&title); err != nil {
				t.Fatal(err)
			}
			if c.absent != "" {
				if title == "" || strings.Contains(title, c.absent) {
					t.Errorf("title = %q, want it set and redacted", title)
				}
				return
			}
			if title != c.want {
				t.Errorf("title = %q, want %q", title, c.want)
			}
		})
	}
}
