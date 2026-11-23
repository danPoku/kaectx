// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package notelink_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/notelink"
	"github.com/danPoku/kaectx/internal/store"
)

// fixtureTitle is the title the save_note call in the fixture carries, and
// fixtureNoteID the id its result returns. The fixture is four lines cut from
// a real Claude Code log around one save_note call, with every value that
// carried content or identity replaced; see testdata/save_note.jsonl in the
// claudecode adapter.
const (
	fixtureTitle   = "Use WAL mode for the catalogue"
	fixtureNoteID  = 13
	fixtureSession = "claude-code:sess-note"
	fixturePath    = "../ingest/claudecode/testdata/save_note.jsonl"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// saveNote puts a note in the catalogue the way save_note would have: with
// no source, because the session was not ingested when it was saved.
func saveNote(t *testing.T, db *sql.DB, id int, title string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO notes(id, kind, title, body) VALUES (?, 'decision', ?, 'body')`, id, title)
}

type source struct {
	session sql.NullString
	anchor  sql.NullInt64
}

func noteSource(t *testing.T, db *sql.DB, id int) source {
	t.Helper()
	var s source
	if err := db.QueryRow(`SELECT session_id, anchor_seq FROM notes WHERE id = ?`, id).Scan(&s.session, &s.anchor); err != nil {
		t.Fatal(err)
	}
	return s
}

// ingestFixture ingests the real-shaped log, or a rewritten copy of it.
func ingestFixture(t *testing.T, db *sql.DB, rewrite func(string) string) string {
	t.Helper()
	b, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if rewrite != nil {
		text = rewrite(text)
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatalf("ingest.Run: %v", err)
	}
	return path
}

// TestIngestLinksNoteToItsExchange is #46's main case: ingesting a Claude
// Code log that holds a save_note call sets the note's session and anchor,
// and ingesting it again changes nothing.
func TestIngestLinksNoteToItsExchange(t *testing.T) {
	db := openDB(t)
	saveNote(t, db, fixtureNoteID, fixtureTitle)
	path := ingestFixture(t, db, nil)

	got := noteSource(t, db, fixtureNoteID)
	if got.session.String != fixtureSession {
		t.Fatalf("session_id = %+v, want %s", got.session, fixtureSession)
	}
	// The anchor is the user turn that asked for the note: the first message
	// of the exchange, and a real user text message.
	var role, kind, content string
	if err := db.QueryRow(`SELECT role, kind, content FROM messages WHERE session_id = ? AND seq = ?`,
		fixtureSession, got.anchor.Int64).Scan(&role, &kind, &content); err != nil {
		t.Fatalf("anchor_seq %+v names no message: %v", got.anchor, err)
	}
	if role != "user" || kind != "text" || !strings.Contains(content, "Save that decision") {
		t.Errorf("anchor message = %s/%s %q, want the user turn that asked for the note", role, kind, content)
	}
	var callSeq int
	if err := db.QueryRow(`SELECT seq FROM messages WHERE session_id = ? AND kind = 'tool_call'`, fixtureSession).Scan(&callSeq); err != nil {
		t.Fatal(err)
	}
	if int(got.anchor.Int64) >= callSeq {
		t.Errorf("anchor_seq %d is not before the call at %d", got.anchor.Int64, callSeq)
	}

	// Idempotent: a second ingest of the same file, and a direct second pass.
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatal(err)
	}
	res, err := notelink.LinkSession(db, fixtureSession)
	if err != nil {
		t.Fatal(err)
	}
	if res.Linked != 0 || res.AlreadyLinked != 1 {
		t.Errorf("second pass = %+v, want nothing linked and one already linked", res)
	}
	if again := noteSource(t, db, fixtureNoteID); again != got {
		t.Errorf("source changed on re-ingest: %+v, was %+v", again, got)
	}
}

// TestLinkLeavesWrongNotesAlone covers every way a logged call can fail to be
// this catalogue's note. Each must leave the note untouched and the ingest
// running to the end.
func TestLinkLeavesWrongNotesAlone(t *testing.T) {
	replace := func(old, new string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, old) {
				panic("fixture does not contain " + old)
			}
			return strings.ReplaceAll(s, old, new)
		}
	}
	cases := []struct {
		name      string
		noteTitle string // title of note 13 in this catalogue; "" = no such note
		rewrite   func(string) string
		want      notelink.Result
	}{
		{
			// A log from another machine: its note 13 was something else.
			name: "same id, different title", noteTitle: "Rotate the API keys monthly",
			want: notelink.Result{Calls: 1, Unmatched: 1},
		},
		{
			name: "no note with that id", noteTitle: "",
			want: notelink.Result{Calls: 1, Unmatched: 1},
		},
		{
			// The tool failed: its result is an error message, not {"id": N}.
			name: "failed call", noteTitle: fixtureTitle,
			rewrite: replace(`{\"id\":13}`, `kind must be one of decision, gotcha, convention, todo, fact`),
			want:    notelink.Result{},
		},
		{
			name: "malformed result", noteTitle: fixtureTitle,
			rewrite: replace(`{\"id\":13}`, `{\"id\":`),
			want:    notelink.Result{},
		},
		{
			name: "result names id zero", noteTitle: fixtureTitle,
			rewrite: replace(`{\"id\":13}`, `{\"id\":0}`),
			want:    notelink.Result{},
		},
		{
			name: "result is a string id", noteTitle: fixtureTitle,
			rewrite: replace(`{\"id\":13}`, `{\"id\":\"13\"}`),
			want:    notelink.Result{},
		},
		{
			// Another MCP server's save_note is matched by name, then refused
			// because its title is not this note's.
			name: "another server's save_note", noteTitle: fixtureTitle,
			rewrite: func(s string) string {
				return replace("mcp__ctx__save_note", "mcp__other__save_note")(replace(fixtureTitle, "Someone else's note")(s))
			},
			want: notelink.Result{Calls: 1, Unmatched: 1},
		},
		{
			name: "a tool that is not save_note", noteTitle: fixtureTitle,
			rewrite: replace("mcp__ctx__save_note", "mcp__ctx__search_notes"),
			want:    notelink.Result{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := openDB(t)
			if c.noteTitle != "" {
				saveNote(t, db, fixtureNoteID, c.noteTitle)
			}
			ingestFixture(t, db, c.rewrite)

			// The whole log was ingested: a bad save_note line stopped nothing.
			var messages int
			var lastErr sql.NullString
			if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = ?`, fixtureSession).Scan(&messages); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT last_error FROM sources LIMIT 1`).Scan(&lastErr); err != nil {
				t.Fatal(err)
			}
			if messages != 4 || lastErr.String != "" {
				t.Errorf("ingested %d messages with last_error %q, want all 4 and no error", messages, lastErr.String)
			}
			if c.noteTitle != "" {
				if got := noteSource(t, db, fixtureNoteID); got.session.Valid || got.anchor.Valid {
					t.Errorf("note was linked: %+v, want it untouched", got)
				}
			}
			res, err := notelink.LinkSession(db, fixtureSession)
			if err != nil {
				t.Fatal(err)
			}
			if res != c.want {
				t.Errorf("LinkSession = %+v, want %+v", res, c.want)
			}
		})
	}
}

// A note already tied to one session is not re-pointed by another session
// that logged the same call (a resumed or copied log).
func TestLinkNeverMovesAnExistingSource(t *testing.T) {
	db := openDB(t)
	saveNote(t, db, fixtureNoteID, fixtureTitle)
	mustExec(t, db, `INSERT INTO sessions(id, agent, native_id, started_at) VALUES ('claude-code:first', 'claude-code', 'first', '2026-09-01T00:00:00Z')`)
	mustExec(t, db, `UPDATE notes SET session_id = 'claude-code:first', anchor_seq = 7 WHERE id = ?`, fixtureNoteID)
	ingestFixture(t, db, nil)
	if got := noteSource(t, db, fixtureNoteID); got.session.String != "claude-code:first" || got.anchor.Int64 != 7 {
		t.Errorf("source = %+v, want the first session's link kept", got)
	}
}

// When one session saves the same title twice, each result belongs to the
// call just before it, so the two notes anchor on their own exchanges.
func TestLinkPairsEachResultWithItsOwnCall(t *testing.T) {
	db := openDB(t)
	mustExec(t, db, `INSERT INTO sessions(id, agent, native_id, started_at) VALUES ('s', 'claude-code', 's', '2026-09-01T00:00:00Z')`)
	mustExec(t, db, `INSERT INTO messages(session_id, seq, role, kind, tool_name, content) VALUES
		('s', 0, 'user', 'text', NULL, 'save a handoff'),
		('s', 1, 'assistant', 'tool_call', 'mcp__ctx__save_note', '{"kind":"todo","title":"Handoff"}'),
		('s', 2, 'tool', 'tool_result', 'mcp__ctx__save_note', '{"id":1}'),
		('s', 3, 'assistant', 'text', NULL, 'saved'),
		('s', 4, 'user', 'text', NULL, 'things changed, save it again'),
		('s', 5, 'assistant', 'tool_call', 'mcp__ctx__save_note', '{"kind":"todo","title":"Handoff"}'),
		('s', 6, 'tool', 'tool_result', 'mcp__ctx__save_note', '{"id":2,"superseded":1}')`)
	saveNote(t, db, 1, "Handoff")
	saveNote(t, db, 2, "Handoff")

	res, err := notelink.LinkSession(db, "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.Linked != 2 {
		t.Fatalf("LinkSession = %+v, want both notes linked", res)
	}
	for id, wantAnchor := range map[int]int64{1: 0, 2: 4} {
		if got := noteSource(t, db, id); got.session.String != "s" || got.anchor.Int64 != wantAnchor {
			t.Errorf("note %d source = %+v, want session s anchored at %d", id, got, wantAnchor)
		}
	}
}

// TestBackfill is the repair path: notes saved, and their sessions ingested,
// before linking existed. A dry run reports the same counts and writes nothing.
func TestBackfill(t *testing.T) {
	db := openDB(t)
	// Ingest first, with no note in the catalogue, so nothing is linked;
	// then the note "appears", as if it had been there all along unlinked.
	ingestFixture(t, db, nil)
	saveNote(t, db, fixtureNoteID, fixtureTitle)
	saveNote(t, db, 99, "A note no log mentions")

	dry, err := notelink.Backfill(db, true)
	if err != nil {
		t.Fatal(err)
	}
	want := notelink.Result{Calls: 1, Linked: 1}
	if dry != want {
		t.Errorf("dry run = %+v, want %+v", dry, want)
	}
	if got := noteSource(t, db, fixtureNoteID); got.session.Valid {
		t.Fatalf("a dry run linked the note: %+v", got)
	}

	real, err := notelink.Backfill(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if real != want {
		t.Errorf("backfill = %+v, want %+v", real, want)
	}
	if got := noteSource(t, db, fixtureNoteID); got.session.String != fixtureSession || !got.anchor.Valid {
		t.Errorf("source after backfill = %+v, want it linked to %s", got, fixtureSession)
	}
	if got := noteSource(t, db, 99); got.session.Valid {
		t.Errorf("a note with no call in any log was linked: %+v", got)
	}
	again, err := notelink.Backfill(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if again != (notelink.Result{Calls: 1, AlreadyLinked: 1}) {
		t.Errorf("second backfill = %+v, want nothing new", again)
	}
}
