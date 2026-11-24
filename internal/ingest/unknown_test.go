// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest_test

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/ingest/codex"
	"github.com/danPoku/kaectx/internal/store"
)

// The unknown_type fixtures follow the shape of real logs, but the line
// types "conversation-digest" and "turn_digest" are made up: a type kae has
// never seen cannot be taken from a real file. The other reported kinds are
// real ones the adapters do not read yet: a Claude Code "document" content
// block (a PDF attached to a prompt), and Codex "function_call" and
// "function_call_output" response items.

// unknownSummary renders ListUnknown's result as sorted lines, one per kind,
// so a test can compare the whole report at once.
func unknownSummary(t *testing.T, db *sql.DB, projectID int64, since string) string {
	t.Helper()
	lines, err := ingest.ListUnknown(db, projectID, since, 50)
	if err != nil {
		t.Fatalf("ListUnknown: %v", err)
	}
	var out []string
	for _, u := range lines {
		out = append(out, fmt.Sprintf("%s|%s|%s|%d lines|%d files|%s|%s", u.Agent, u.LineType, u.Version, u.Lines, u.Files, u.FirstSeen, u.LastSeen))
	}
	return strings.Join(out, "\n")
}

const wantClaudeUnknown = "claude-code|content/document|9.9.0|1 lines|1 files|2026-11-20T10:06:00Z|2026-11-20T10:06:00Z\n" +
	"claude-code|conversation-digest|9.9.0|2 lines|1 files|2026-11-20T10:00:06Z|2026-11-20T10:05:00Z"

func TestUnknownLineTypesAreCountedOncePerLine(t *testing.T) {
	db := openTestDB(t)
	path := copyFixture(t, "claudecode/testdata/unknown_type.jsonl")
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The new type and the unread content block are reported with the agent,
	// its version, the count and the first and last time. The known
	// bookkeeping types in the same file (mode, file-history-snapshot) are
	// not.
	if got := unknownSummary(t, db, 0, ""); got != wantClaudeUnknown {
		t.Fatalf("after first ingest:\n%s\nwant:\n%s", got, wantClaudeUnknown)
	}
	// The conversation around the unknown lines is still stored.
	var messages int
	db.QueryRow(`SELECT count(*) FROM messages`).Scan(&messages)
	if messages != 3 {
		t.Fatalf("stored %d messages, want the 3 readable ones", messages)
	}

	// Re-ingesting the same file reads no new bytes and must not double the
	// counts.
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if got := unknownSummary(t, db, 0, ""); got != wantClaudeUnknown {
		t.Fatalf("re-ingest changed the counts:\n%s", got)
	}

	// A line appended later is added to the existing count.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"conversation-digest","digest":"later","sessionId":"sess-u","version":"9.9.0","timestamp":"2026-11-20T11:00:00Z"}`)
	f.Close()
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatalf("Run after append: %v", err)
	}
	want := strings.Replace(wantClaudeUnknown, "2 lines|1 files|2026-11-20T10:00:06Z|2026-11-20T10:05:00Z", "3 lines|1 files|2026-11-20T10:00:06Z|2026-11-20T11:00:00Z", 1)
	// The digest was seen most recently now, so it sorts first.
	wantLines := strings.Split(want, "\n")
	want = wantLines[1] + "\n" + wantLines[0]
	if got := unknownSummary(t, db, 0, ""); got != want {
		t.Fatalf("after append:\n%s\nwant:\n%s", got, want)
	}
}

// A rewritten file is read again from its first byte. Its lines must replace
// the old counts, not be added to them.
func TestUnknownLineTypesAreRecountedWhenAFileIsRewritten(t *testing.T) {
	db := openTestDB(t)
	path := copyFixture(t, "claudecode/testdata/unknown_type.jsonl")
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Rewrite with a different first line (so the stored head hash no longer
	// matches) and only one unknown line left.
	rewritten := `{"type":"mode","mode":"plan","sessionId":"sess-u"}
{"type":"user","message":{"role":"user","content":"Summarise the attached quarterly report."},"sessionId":"sess-u","cwd":"/home/dev/proj","version":"9.9.0","timestamp":"2026-11-20T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Revenue grew."}]},"sessionId":"sess-u","cwd":"/home/dev/proj","version":"9.9.0","timestamp":"2026-11-20T10:00:05Z"}
{"type":"conversation-digest","digest":"report summary","sessionId":"sess-u","version":"9.9.0","timestamp":"2026-11-20T10:00:06Z"}
`
	if err := os.WriteFile(path, []byte(rewritten), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatalf("Run after rewrite: %v", err)
	}
	want := "claude-code|conversation-digest|9.9.0|1 lines|1 files|2026-11-20T10:00:06Z|2026-11-20T10:00:06Z"
	if got := unknownSummary(t, db, 0, ""); got != want {
		t.Fatalf("after rewrite:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnknownCodexItemsAreReportedButEventBroadcastsAreNot(t *testing.T) {
	db := openTestDB(t)
	path := copyFixture(t, "codex/testdata/unknown_type.jsonl")
	if err := ingest.Run(db, codex.New(), path); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Reported: the new top-level type, and response items the adapter does
	// not read. Not reported: world_state (known bookkeeping) and the new
	// event_msg subtype, which only repeats what response items record.
	want := "codex|turn_digest|9.9.0|1 lines|1 files|2026-11-21T09:00:04.000Z|2026-11-21T09:00:04.000Z\n" +
		"codex|response_item/function_call|9.9.0|2 lines|1 files|2026-11-21T09:00:01.000Z|2026-11-21T09:00:03.000Z\n" +
		"codex|response_item/function_call_output|9.9.0|1 lines|1 files|2026-11-21T09:00:02.000Z|2026-11-21T09:00:02.000Z"
	if got := unknownSummary(t, db, 0, ""); got != want {
		t.Fatalf("codex unknown lines:\n%s\nwant:\n%s", got, want)
	}

	// A run that resumes mid-file gets a fresh adapter; it must still know
	// the CLI version, which only the first line carries.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"timestamp":"2026-11-21T09:10:00.000Z","ordinal":10,"type":"turn_digest","payload":{"turn_id":"turn-2"}}`)
	f.Close()
	if err := ingest.Run(db, codex.New(), path); err != nil {
		t.Fatalf("Run after append: %v", err)
	}
	if got := unknownSummary(t, db, 0, ""); !strings.Contains(got, "codex|turn_digest|9.9.0|2 lines|1 files|2026-11-21T09:00:04.000Z|2026-11-21T09:10:00.000Z") {
		t.Fatalf("resumed run lost the version or the count:\n%s", got)
	}
}

func TestListUnknownByProjectAndAge(t *testing.T) {
	db := openTestDB(t)
	path := copyFixture(t, "claudecode/testdata/unknown_type.jsonl")
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatalf("Run: %v", err)
	}
	own, err := store.ResolveProject(db, "/home/dev/proj")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.ResolveProject(db, "/home/dev/another")
	if err != nil {
		t.Fatal(err)
	}

	if got := unknownSummary(t, db, own, ""); got != wantClaudeUnknown {
		t.Errorf("the session's own project should see its unknown lines:\n%s", got)
	}
	if got := unknownSummary(t, db, other, ""); got != "" {
		t.Errorf("another project must not be told about them:\n%s", got)
	}
	// Types last seen before the cut-off are left out; the fixture's lines
	// are all from 20 November.
	if got := unknownSummary(t, db, own, "2026-11-21T00:00:00Z"); got != "" {
		t.Errorf("lines older than the cut-off were listed:\n%s", got)
	}
	if got := unknownSummary(t, db, own, "2026-11-20T10:06:00Z"); !strings.HasPrefix(got, "claude-code|content/document") || strings.Contains(got, "digest") {
		t.Errorf("only the type seen at or after the cut-off should remain:\n%s", got)
	}
}
