// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/store"
)

type briefOut struct {
	Text          string `json:"text"`
	Tokens        int    `json:"tokens"`
	Budget        int    `json:"budget"`
	HandoffNoteID int64  `json:"handoff_note_id"`
}

// get_brief returns the same brief the session-start hook prints, within the
// budget asked for, and reading a handoff through it is recorded.
func TestGetBrief(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	const cwd = "/home/dev/proj"
	projectID, err := store.ResolveProject(db, cwd)
	if err != nil {
		t.Fatal(err)
	}
	cs := serverAt(t, db, cwd)

	// No history yet: an empty brief, not an error and not a bare header.
	var out briefOut
	callTool(t, cs, "get_brief", map[string]any{}, &out)
	if out.Text != "" || out.Tokens != 0 || out.Budget != 600 {
		t.Fatalf("empty project: %+v", out)
	}

	if _, err := db.Exec(`INSERT INTO sessions(id, agent, native_id, project_id, started_at, title, message_count)
		VALUES ('codex:s1', 'codex', 's1', ?, '2026-11-22T09:00:00Z', 'Add rate limiting to the login route', 30)`, projectID); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO notes(project_id, agent, kind, title, body, tags)
		VALUES (?, 'codex', 'todo', 'Handoff: rate limiting', 'Limiter is in; the Redis store is not wired yet.', '["handoff"]')`, projectID)
	if err != nil {
		t.Fatal(err)
	}
	noteID, _ := res.LastInsertId()

	out = briefOut{}
	callTool(t, cs, "get_brief", map[string]any{}, &out)
	for _, want := range []string{"Handoff: rate limiting", "Redis store", `get_session(session_id="codex:s1")`} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("brief lacks %q:\n%s", want, out.Text)
		}
	}
	if out.HandoffNoteID != noteID || out.Tokens == 0 || out.Tokens > out.Budget {
		t.Errorf("unexpected result: %+v", out)
	}
	var deliveries int
	db.QueryRow(`SELECT count(*) FROM note_retrievals WHERE note_id = ?`, noteID).Scan(&deliveries)
	if deliveries != 1 {
		t.Errorf("the handoff's delivery was recorded %d times, want 1", deliveries)
	}

	// A small budget is respected.
	out = briefOut{}
	callTool(t, cs, "get_brief", map[string]any{"budget": 80}, &out)
	if out.Budget != 80 || out.Tokens > 80 {
		t.Errorf("budget 80 gave %d tokens:\n%s", out.Tokens, out.Text)
	}
}
