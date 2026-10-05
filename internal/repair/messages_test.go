// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package repair

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/store"
)

// seedOldStyleSession stores a session the way earlier builds did: a
// task-notification filed as a user turn (so it opened its own chunk), a
// prompt cut to its first block (the IDE notice), and the injected text as
// the session title.
func seedOldStyleSession(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}

	const notice = "<ide_opened_file>The user opened the file a.go in the IDE.</ide_opened_file>"
	if _, err := db.Exec(`INSERT INTO sessions(id, agent, native_id, started_at, title) VALUES ('claude-code:s1','claude-code','s1','2026-09-20T10:00:00Z', ?)`, notice); err != nil {
		t.Fatal(err)
	}
	msgs := []struct {
		role, kind, content, raw string
	}{
		{"user", "text", notice,
			`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"` + notice + `"},{"type":"text","text":"why does embed time out?"}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:00Z"}`},
		{"assistant", "text", "Ollama is cold-starting.",
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Ollama is cold-starting."}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:05Z"}`},
		{"user", "text", "<task-notification>\n<summary>Monitor event</summary>\n</task-notification>",
			`{"type":"user","message":{"role":"user","content":"<task-notification>\n<summary>Monitor event</summary>\n</task-notification>"},"sessionId":"s1","timestamp":"2026-09-20T10:01:00Z"}`},
		{"assistant", "text", "Noted, still waiting.",
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Noted, still waiting."}]},"sessionId":"s1","timestamp":"2026-09-20T10:01:05Z"}`},
	}
	for i, m := range msgs {
		if _, err := db.Exec(`INSERT INTO messages(session_id, seq, role, kind, content, created_at, raw) VALUES ('claude-code:s1', ?, ?, ?, ?, '2026-09-20T10:00:00Z', ?)`,
			i, m.role, m.kind, m.content, m.raw); err != nil {
			t.Fatal(err)
		}
	}
	// The chunks the old classification produced: one per "user turn".
	for _, c := range []struct {
		first, last int
		text        string
	}{{0, 1, "User: " + notice + "\n\nAssistant: Ollama is cold-starting."}, {2, 3, "User: <task-notification>...\n\nAssistant: Noted, still waiting."}} {
		if _, err := db.Exec(`INSERT INTO chunks(session_id, first_seq, last_seq, text) VALUES ('claude-code:s1', ?, ?, ?)`, c.first, c.last, c.text); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestMessagesRepairReclassifiesRechunksAndRetitles(t *testing.T) {
	db := seedOldStyleSession(t)

	res, err := Messages(db, false)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if res.Reclassed != 1 || res.Recovered != 1 || res.Sessions != 1 || res.Titled != 1 {
		t.Errorf("result = %+v, want 1 reclassed, 1 recovered, 1 session rechunked, 1 retitled", res)
	}

	var role, content string
	db.QueryRow(`SELECT role, content FROM messages WHERE seq = 0`).Scan(&role, &content)
	if role != "user" || content != "why does embed time out?" {
		t.Errorf("seq 0 = %s %q, want the typed prompt recovered", role, content)
	}
	db.QueryRow(`SELECT role FROM messages WHERE seq = 2`).Scan(&role)
	if role != "system" {
		t.Errorf("task-notification role = %s, want system", role)
	}

	// One exchange now, the notification folded in as a non-boundary event.
	rows, err := db.Query(`SELECT first_seq, last_seq, text FROM chunks ORDER BY first_seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var chunks []string
	for rows.Next() {
		var f, l int
		var text string
		rows.Scan(&f, &l, &text)
		chunks = append(chunks, text)
		if f != 0 || l != 3 {
			t.Errorf("chunk spans %d-%d, want 0-3", f, l)
		}
	}
	if len(chunks) != 1 || strings.Contains(chunks[0], "task-notification") || !strings.Contains(chunks[0], "why does embed time out?") {
		t.Errorf("chunks = %q, want one chunk with the typed prompt and no notification", chunks)
	}

	var title string
	db.QueryRow(`SELECT title FROM sessions`).Scan(&title)
	if title != "why does embed time out?" {
		t.Errorf("title = %q, want the typed prompt", title)
	}

	again, err := Messages(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Reclassed+again.Recovered+again.Expanded+again.Sessions+again.Titled != 0 {
		t.Errorf("second run = %+v, want nothing left to do", again)
	}
}

// Earlier builds titled sessions from the raw prompt before redaction, so a
// pasted key could sit in an otherwise fine title.
func TestMessagesRepairRedactsKeptTitles(t *testing.T) {
	db := seedOldStyleSession(t)
	const key = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	if _, err := db.Exec(`INSERT INTO sessions(id, agent, native_id, started_at, title) VALUES ('codex:s2','codex','s2','2026-09-20T10:00:00Z', ?)`,
		"use ANTHROPIC_API_KEY="+key+" here"); err != nil {
		t.Fatal(err)
	}
	if _, err := Messages(db, false); err != nil {
		t.Fatal(err)
	}
	var title string
	db.QueryRow(`SELECT title FROM sessions WHERE id = 'codex:s2'`).Scan(&title)
	if title == "" || strings.Contains(title, key) {
		t.Errorf("title = %q, want it kept but redacted", title)
	}
}

func TestMessagesRepairDryRunWritesNothing(t *testing.T) {
	db := seedOldStyleSession(t)
	res, err := Messages(db, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reclassed == 0 {
		t.Fatalf("dry run found nothing to do: %+v", res)
	}
	var users, chunks int
	db.QueryRow(`SELECT COUNT(*) FROM messages WHERE role = 'user'`).Scan(&users)
	db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunks)
	if users != 2 || chunks != 2 {
		t.Errorf("after dry run: %d user messages, %d chunks; want the original 2 and 2", users, chunks)
	}
}
