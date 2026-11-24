// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/ingest/codex"
	"github.com/danPoku/kaectx/internal/store"
)

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// appendLine adds one line to a log file, as the agent would when the
// session continues.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

const laterClaudeTurn = `{"type":"user","message":{"role":"user","content":"One more question about checkpoints."},"sessionId":"sess-1","cwd":"/home/dev/proj","timestamp":"2026-09-20T12:00:00Z"}`

// A forgotten session does not come back: not on the next ingest of the
// unchanged file, not when the session continues, not when the log file is
// rewritten, and not through a walk of the whole log directory.
func TestForgottenSessionIsNeverIngestedAgain(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	fixture, err := os.ReadFile("claudecode/testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture, 0644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatal(err)
	}
	const id = "claude-code:sess-1"
	if count(t, db, `SELECT count(*) FROM messages WHERE session_id = ?`, id) == 0 {
		t.Fatal("fixture session was not ingested")
	}

	got, err := store.ForgetSession(db, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Messages == 0 || got.Chunks == 0 {
		t.Fatalf("forget reported nothing removed: %+v", got)
	}
	gone := func(when string) {
		t.Helper()
		for table, query := range map[string]string{
			"sessions":     `SELECT count(*) FROM sessions WHERE id = ?`,
			"messages":     `SELECT count(*) FROM messages WHERE session_id = ?`,
			"chunks":       `SELECT count(*) FROM chunks WHERE session_id = ?`,
			"file_touches": `SELECT count(*) FROM file_touches WHERE session_id = ?`,
		} {
			if n := count(t, db, query, id); n != 0 {
				t.Fatalf("%s: %d row(s) in %s for the forgotten session", when, n, table)
			}
		}
		// The search index has no entry left that could surface its text.
		if n := count(t, db, `SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'WAL'`); n != 0 {
			t.Fatalf("%s: the forgotten session is still searchable", when)
		}
	}
	gone("right after forget")

	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatal(err)
	}
	gone("after re-ingesting the unchanged file")

	appendLine(t, path, laterClaudeTurn)
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatal(err)
	}
	gone("after the session continued")

	// Rewritten from the first byte: ingest starts the file over.
	if err := os.WriteFile(path, append([]byte(`{"type":"mode","mode":"plan","sessionId":"sess-1"}`+"\n"), fixture...), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatal(err)
	}
	gone("after the log file was rewritten")

	// A daemon rescan walks the directory; same answer.
	if _, err := ingest.RunAll(db, func() ingest.Adapter { return claudecode.New() }, dir); err != nil {
		t.Fatal(err)
	}
	gone("after a directory rescan")

	// Forgetting it twice is an error, not a silent success.
	if _, err := store.ForgetSession(db, id); err == nil {
		t.Fatal("forgetting a session that is already gone should fail")
	}
}

// Forgetting one session leaves the others, and notes saved in it, alone.
func TestForgetLeavesOtherSessionsAndNotes(t *testing.T) {
	db := openTestDB(t)
	if err := ingest.Run(db, claudecode.New(), copyFixture(t, "claudecode/testdata/session.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, codex.New(), copyFixture(t, "codex/testdata/session.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notes(project_id, session_id, kind, title) VALUES (1, 'claude-code:sess-1', 'decision', 'use WAL')`); err != nil {
		t.Fatal(err)
	}
	codexBefore := count(t, db, `SELECT count(*) FROM messages WHERE session_id = 'codex:sess-1'`)

	if _, err := store.ForgetSession(db, "claude-code:sess-1"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM messages WHERE session_id = 'codex:sess-1'`); n != codexBefore || n == 0 {
		t.Fatalf("the other session's messages went from %d to %d", codexBefore, n)
	}
	if n := count(t, db, `SELECT count(*) FROM notes WHERE title = 'use WAL' AND session_id IS NULL`); n != 1 {
		t.Fatal("a note saved in the forgotten session should be kept, with its link cleared")
	}
}

// An excluded project's new sessions are not stored, a session it already
// holds stops growing, and including it again reads what was passed over.
func TestExcludedProjectIsNotIngested(t *testing.T) {
	db := openTestDB(t)
	claudePath := copyFixture(t, "claudecode/testdata/session.jsonl")
	if err := ingest.Run(db, claudecode.New(), claudePath); err != nil {
		t.Fatal(err)
	}
	projectID, found, err := store.LookupProject(db, "/home/dev/proj")
	if err != nil || !found {
		t.Fatalf("fixture project missing: %v", err)
	}
	before := count(t, db, `SELECT count(*) FROM messages`)

	if changed, err := store.SetProjectExcluded(db, projectID, true); err != nil || !changed {
		t.Fatalf("exclude: changed=%v err=%v", changed, err)
	}
	if changed, _ := store.SetProjectExcluded(db, projectID, true); changed {
		t.Fatal("excluding twice should report no change the second time")
	}

	// A new session in the excluded project (the Codex fixture ran in the
	// same folder): nothing is stored, not even a session row.
	codexPath := copyFixture(t, "codex/testdata/session.jsonl")
	if err := ingest.Run(db, codex.New(), codexPath); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM sessions WHERE agent = 'codex'`); n != 0 {
		t.Fatalf("a new session of an excluded project was stored (%d)", n)
	}
	// The session already held stops growing.
	appendLine(t, claudePath, laterClaudeTurn)
	if err := ingest.Run(db, claudecode.New(), claudePath); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM messages`); n != before {
		t.Fatalf("messages went from %d to %d while the project was excluded", before, n)
	}
	// No project was created for the excluded folder under another name.
	if n := count(t, db, `SELECT count(*) FROM projects`); n != 1 {
		t.Fatalf("%d projects, want 1", n)
	}

	// A session in a different folder is unaffected.
	other := filepath.Join(t.TempDir(), "other.jsonl")
	otherLog := `{"type":"user","message":{"role":"user","content":"Unrelated work elsewhere."},"sessionId":"sess-other","cwd":"/home/dev/elsewhere","timestamp":"2026-09-21T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]},"sessionId":"sess-other","cwd":"/home/dev/elsewhere","timestamp":"2026-09-21T10:00:05Z"}
`
	if err := os.WriteFile(other, []byte(otherLog), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, claudecode.New(), other); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM sessions WHERE id = 'claude-code:sess-other'`); n != 1 {
		t.Fatal("a session outside the excluded project was not stored")
	}

	// Include it again: the next ingest reads what was passed over, once.
	if changed, err := store.SetProjectExcluded(db, projectID, false); err != nil || !changed {
		t.Fatalf("include: changed=%v err=%v", changed, err)
	}
	for range 2 {
		if err := ingest.Run(db, codex.New(), codexPath); err != nil {
			t.Fatal(err)
		}
		if err := ingest.Run(db, claudecode.New(), claudePath); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM sessions WHERE agent = 'codex'`); n != 1 {
		t.Fatalf("after including the project its skipped session should be ingested, got %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM messages WHERE session_id = 'claude-code:sess-1' AND content LIKE '%One more question%'`); n != 1 {
		t.Fatalf("the line skipped while excluded should be stored exactly once, got %d", n)
	}
	// Re-reading the Claude Code file replaced its rows; it did not double them.
	if n := count(t, db, `SELECT count(*) FROM messages WHERE session_id = 'claude-code:sess-1'`); n != before+1 {
		t.Fatalf("claude session has %d messages, want %d", n, before+1)
	}
}
