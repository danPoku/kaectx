// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// saveNote saves a note through the real tool and returns its ID. Every note
// in these tests shares the word "deploy" so one search_notes call can tell
// which of them are still live.
func saveNote(t *testing.T, cs *sdkmcp.ClientSession, title string, supersedes int64) int64 {
	t.Helper()
	args := map[string]any{"kind": "decision", "title": title, "body": "deploy notes"}
	if supersedes != 0 {
		args["supersedes"] = supersedes
	}
	var out struct {
		ID         int64 `json:"id"`
		Superseded int64 `json:"superseded"`
	}
	callTool(t, cs, "save_note", args, &out)
	if out.ID == 0 {
		t.Fatalf("save_note(%q) returned id=0", title)
	}
	if out.Superseded != supersedes {
		t.Fatalf("save_note(%q) superseded = %d, want %d", title, out.Superseded, supersedes)
	}
	return out.ID
}

func liveNoteIDs(t *testing.T, cs *sdkmcp.ClientSession) []int64 {
	t.Helper()
	var out struct {
		Notes []struct {
			ID int64 `json:"id"`
		} `json:"notes"`
	}
	callTool(t, cs, "search_notes", map[string]any{"query": "deploy"}, &out)
	ids := make([]int64, len(out.Notes))
	for i, n := range out.Notes {
		ids[i] = n.ID
	}
	return ids
}

func supersededBy(t *testing.T, db *sql.DB, id int64) sql.NullInt64 {
	t.Helper()
	var v sql.NullInt64
	if err := db.QueryRow(`SELECT superseded_by FROM notes WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("read superseded_by of note %d: %v", id, err)
	}
	return v
}

func noteCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSaveNoteSupersedes: saving B with supersedes=A retires A, and a search
// that matches both returns only B.
func TestSaveNoteSupersedes(t *testing.T) {
	cs, db := testServer(t)
	a := saveNote(t, cs, "Deploy on Fridays", 0)
	if got := supersededBy(t, db, a); got.Valid {
		t.Fatalf("a note saved without supersedes has superseded_by = %d, want NULL", got.Int64)
	}
	b := saveNote(t, cs, "Deploy on Mondays", a)

	if got := supersededBy(t, db, a); !got.Valid || got.Int64 != b {
		t.Errorf("A.superseded_by = %+v, want %d", got, b)
	}
	if ids := liveNoteIDs(t, cs); len(ids) != 1 || ids[0] != b {
		t.Errorf("search_notes returned %v, want only B (%d)", ids, b)
	}
}

// TestSaveNoteSupersedesChain: a handoff replaced twice leaves each link
// pointing at the next one, A -> B -> C, and only C is live.
func TestSaveNoteSupersedesChain(t *testing.T) {
	cs, db := testServer(t)
	a := saveNote(t, cs, "Handoff 1", 0)
	b := saveNote(t, cs, "Handoff 2", a)
	c := saveNote(t, cs, "Handoff 3", b)

	if got := supersededBy(t, db, a); !got.Valid || got.Int64 != b {
		t.Errorf("A.superseded_by = %+v, want %d", got, b)
	}
	if got := supersededBy(t, db, b); !got.Valid || got.Int64 != c {
		t.Errorf("B.superseded_by = %+v, want %d", got, c)
	}
	if ids := liveNoteIDs(t, cs); len(ids) != 1 || ids[0] != c {
		t.Errorf("search_notes returned %v, want only C (%d)", ids, c)
	}
}

// TestSaveNoteRejectsBadSupersedes covers every supersedes value that must be
// refused. Each case must leave the notes table exactly as it found it: no new
// note, and no change to the note that was named.
func TestSaveNoteRejectsBadSupersedes(t *testing.T) {
	cs, db := testServer(t)

	// A chain in this project: old -> mid -> live.
	old := saveNote(t, cs, "Deploy v1", 0)
	mid := saveNote(t, cs, "Deploy v2", old)
	live := saveNote(t, cs, "Deploy v3", mid)

	// A note owned by another project, and a global one (project_id NULL).
	res, err := db.Exec(`INSERT INTO projects(name) VALUES ('elsewhere')`)
	if err != nil {
		t.Fatal(err)
	}
	otherProject, _ := res.LastInsertId()
	res, err = db.Exec(`INSERT INTO notes(project_id, kind, title, body) VALUES (?, 'decision', 'Their deploy', 'deploy')`, otherProject)
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := res.LastInsertId()
	res, err = db.Exec(`INSERT INTO notes(project_id, kind, title, body) VALUES (NULL, 'decision', 'Global deploy', 'deploy')`)
	if err != nil {
		t.Fatal(err)
	}
	global, _ := res.LastInsertId()

	cases := []struct {
		name       string
		supersedes int64
		wantInErr  string
	}{
		{"missing note", 99999, "not found"},
		{"negative id", -1, "must be a note ID"},
		{"another project's note", foreign, "another project"},
		{"global note", global, "another project"},
		{"already superseded", mid, "live successor is note " + strconv.FormatInt(live, 10)},
		{"start of a chain names the live end, not the next link", old, "live successor is note " + strconv.FormatInt(live, 10)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := noteCount(t, db)
			res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{
				Name:      "save_note",
				Arguments: map[string]any{"kind": "decision", "title": "Rejected", "supersedes": c.supersedes},
			})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if !res.IsError {
				t.Fatalf("save_note(supersedes=%d) succeeded, want a tool error", c.supersedes)
			}
			msg := ""
			if len(res.Content) > 0 {
				if tc, ok := res.Content[0].(*sdkmcp.TextContent); ok {
					msg = tc.Text
				}
			}
			if !strings.Contains(msg, c.wantInErr) {
				t.Errorf("error = %q, want it to contain %q", msg, c.wantInErr)
			}
			if after := noteCount(t, db); after != before {
				t.Errorf("notes went from %d to %d rows; a rejected save must insert nothing", before, after)
			}
		})
	}

	// The rejected calls must not have disturbed the chain or the foreign notes.
	if got := supersededBy(t, db, old); !got.Valid || got.Int64 != mid {
		t.Errorf("old.superseded_by = %+v, want %d", got, mid)
	}
	if got := supersededBy(t, db, mid); !got.Valid || got.Int64 != live {
		t.Errorf("mid.superseded_by = %+v, want %d", got, live)
	}
	for _, id := range []int64{live, foreign, global} {
		if got := supersededBy(t, db, id); got.Valid {
			t.Errorf("note %d.superseded_by = %d, want NULL", id, got.Int64)
		}
	}
}
