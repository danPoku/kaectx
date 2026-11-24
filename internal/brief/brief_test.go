// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package brief

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danPoku/kaectx/internal/store"
)

var now = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	id, _ := res.LastInsertId()
	return id
}

// seed builds a project with sessions from two agents, file touches inside
// and outside the past week, and returns its id.
func seed(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	p := exec(t, db, `INSERT INTO projects(name) VALUES ('api')`)
	sessions := []struct {
		id, agent, title, at string
		parent               any
	}{
		{"claude-code:a", "claude-code", "Fix the refresh token race", "2026-11-23T10:00:00Z", nil},
		{"codex:b", "codex", "Add rate limiting to the login route", "2026-11-22T09:00:00Z", nil},
		{"claude-code:old", "claude-code", "Bootstrap the repo", "2026-10-01T09:00:00Z", nil},
		// A subagent transcript: never listed as a session of its own.
		{"claude-code:sub", "claude-code", "subagent prompt", "2026-11-23T11:00:00Z", "claude-code:a"},
	}
	for _, s := range sessions {
		exec(t, db, `INSERT INTO sessions(id, agent, native_id, project_id, parent_session_id, started_at, ended_at, title, message_count)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 12)`, s.id, s.agent, s.id, p, s.parent, s.at, s.at, s.title)
	}
	touch := func(session, path, action, at string) {
		exec(t, db, `INSERT INTO file_touches(session_id, project_id, path, action, created_at) VALUES (?, ?, ?, ?, ?)`, session, p, path, action, at)
	}
	touch("claude-code:a", "internal/auth/token.go", "edit", "2026-11-23T10:01:00Z")
	touch("claude-code:a", "internal/auth/token.go", "edit", "2026-11-23T10:02:00Z")
	touch("claude-code:a", "internal/auth/token_test.go", "create", "2026-11-23T10:03:00Z")
	touch("claude-code:a", "README.md", "read", "2026-11-23T10:04:00Z") // reads are not changes
	touch("codex:b", "internal/http/login.go", "edit", "2026-11-22T09:01:00Z")
	touch("claude-code:old", "go.mod", "create", "2026-10-01T09:01:00Z") // outside the week
	return p
}

func addHandoff(t *testing.T, db *sql.DB, project int64, title, body, at string) int64 {
	t.Helper()
	return exec(t, db, `INSERT INTO notes(project_id, agent, kind, title, body, tags, created_at)
		VALUES (?, 'claude-code', 'todo', ?, ?, '["handoff"]', ?)`, project, title, body, at)
}

func TestBuildAndRender(t *testing.T) {
	db := openDB(t)
	p := seed(t, db)
	note := addHandoff(t, db, p, "Handoff: token race", "Mutex added around refresh. Next: add the regression test for two concurrent refreshes.", "2026-11-23T12:00:00Z")

	b, err := Build(db, p, now)
	if err != nil {
		t.Fatal(err)
	}
	r := b.Render(DefaultBudget)
	for _, want := range []string{
		"kae brief: api (about ",
		" of 600 tokens)",
		fmt.Sprintf("Latest handoff (note %d, 2026-11-23, saved by claude-code): Handoff: token race", note),
		"Next: add the regression test",
		`- 2026-11-23 claude-code: Fix the refresh token race [internal/auth/token.go, internal/auth/token_test.go]  get_session(session_id="claude-code:a")`,
		`- 2026-11-22 codex: Add rate limiting to the login route [internal/http/login.go]  get_session(session_id="codex:b")`,
		`- internal/auth/token.go (2)  explain_file(path="internal/auth/token.go")`,
	} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("brief lacks %q:\n%s", want, r.Text)
		}
	}
	for _, unwanted := range []string{"subagent prompt", "README.md", "go.mod ("} {
		if strings.Contains(r.Text, unwanted) {
			t.Errorf("brief should not mention %q:\n%s", unwanted, r.Text)
		}
	}
	if r.HandoffNoteID != note {
		t.Errorf("HandoffNoteID = %d, want %d", r.HandoffNoteID, note)
	}
	if r.Tokens > r.Budget || r.Tokens != Tokens(r.Text) {
		t.Errorf("tokens %d, budget %d, actual %d", r.Tokens, r.Budget, Tokens(r.Text))
	}
}

// The brief must never exceed its budget, whatever the budget and however
// long the stored text is.
func TestRenderNeverExceedsBudget(t *testing.T) {
	db := openDB(t)
	p := seed(t, db)
	addHandoff(t, db, p, strings.Repeat("long title ", 60), strings.Repeat("A very long handoff body. ", 400), "2026-11-23T12:00:00Z")
	b, err := Build(db, p, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int{1, 10, 40, 60, 80, 120, 200, 300, 600, 2000} {
		r := b.Render(budget)
		if got := Tokens(r.Text); got > budget {
			t.Errorf("budget %d: brief is %d tokens:\n%s", budget, got, r.Text)
		}
		if r.Text == "" && r.HandoffNoteID != 0 {
			t.Errorf("budget %d: a handoff that was not printed is reported as delivered", budget)
		}
		if r.Text != "" && !strings.HasPrefix(r.Text, "kae brief: api") {
			t.Errorf("budget %d: text without its header:\n%s", budget, r.Text)
		}
	}
}

func TestEmptyProjectRendersNothing(t *testing.T) {
	db := openDB(t)
	p := exec(t, db, `INSERT INTO projects(name) VALUES ('new')`)
	b, err := Build(db, p, now)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Empty() {
		t.Fatalf("a project with no history should build an empty brief: %+v", b)
	}
	if r := b.Render(DefaultBudget); r.Text != "" || r.Tokens != 0 || r.HandoffNoteID != 0 {
		t.Fatalf("empty brief must be empty, not padded: %+v", r)
	}
	// A project id that does not exist is the same: nothing, no error.
	b, err = Build(db, 9999, now)
	if err != nil || !b.Empty() {
		t.Fatalf("unknown project: %+v, %v", b, err)
	}
}

func TestLatestHandoff(t *testing.T) {
	db := openDB(t)
	p := seed(t, db)
	other := exec(t, db, `INSERT INTO projects(name) VALUES ('web')`)

	if h, err := LatestHandoff(db, p); err != nil || h != nil {
		t.Fatalf("no handoff yet: %+v, %v", h, err)
	}

	// An ordinary note is not a handoff, and nor is another project's.
	exec(t, db, `INSERT INTO notes(project_id, kind, title, created_at) VALUES (?, 'decision', 'use pgx', '2026-11-23T15:00:00Z')`, p)
	addHandoff(t, db, other, "web handoff", "", "2026-11-23T16:00:00Z")
	if h, _ := LatestHandoff(db, p); h != nil {
		t.Fatalf("an untagged note or another project's handoff was returned: %+v", h)
	}

	first := addHandoff(t, db, p, "first handoff", "", "2026-11-20T10:00:00Z")
	second := addHandoff(t, db, p, "second handoff", "", "2026-11-21T10:00:00Z")
	if h, _ := LatestHandoff(db, p); h == nil || h.NoteID != second {
		t.Fatalf("want the newest handoff %d, got %+v", second, h)
	}
	// A superseded handoff gives way to the live one before it.
	exec(t, db, `UPDATE notes SET superseded_by = ? WHERE id = ?`, first, second)
	if h, _ := LatestHandoff(db, p); h == nil || h.NoteID != first {
		t.Fatalf("want the live handoff %d, got %+v", first, h)
	}

	// A note with no tag is a handoff when a checkpoint recorded it as one.
	exec(t, db, `UPDATE notes SET superseded_by = NULL WHERE id = ?`, second)
	linked := exec(t, db, `INSERT INTO notes(project_id, kind, title, created_at, session_id, anchor_seq)
		VALUES (?, 'todo', 'checkpoint handoff', '2026-11-23T18:00:00Z', 'claude-code:a', 7)`, p)
	exec(t, db, `INSERT INTO checkpoint_observations(session_id, project_id, level, message_count, duration_minutes, files_changed, outcome, note_id)
		VALUES ('claude-code:a', ?, 'checkpoint_recommended', 400, 240, 3, 'saved_handoff', ?)`, p, linked)
	h, err := LatestHandoff(db, p)
	if err != nil || h == nil || h.NoteID != linked {
		t.Fatalf("want the checkpoint's handoff %d, got %+v, %v", linked, h, err)
	}
	if !h.HasSource || h.SessionID != "claude-code:a" || h.AnchorSeq != 7 {
		t.Fatalf("source not carried: %+v", h)
	}
	b, _ := Build(db, p, now)
	if text := b.Render(DefaultBudget).Text; !strings.Contains(text, `saved in: get_session(session_id="claude-code:a", from_seq=7)`) {
		t.Fatalf("brief does not point at the handoff's source:\n%s", text)
	}
}

func TestRecordDelivery(t *testing.T) {
	db := openDB(t)
	p := seed(t, db)
	note := addHandoff(t, db, p, "handoff", "", "2026-11-23T12:00:00Z")

	if err := RecordDelivery(db, 0); err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM note_retrievals`).Scan(&n)
	if n != 0 {
		t.Fatal("a brief with no handoff recorded a delivery")
	}
	for range 2 {
		if err := RecordDelivery(db, note); err != nil {
			t.Fatal(err)
		}
	}
	db.QueryRow(`SELECT count(*) FROM note_retrievals WHERE note_id = ? AND rank = 1`, note).Scan(&n)
	if n != 2 {
		t.Fatalf("want one row per delivery, got %d", n)
	}
}
