// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/doctor"
	"github.com/danPoku/kaectx/internal/handoff"
	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/ingest/codex"
	"github.com/danPoku/kaectx/internal/store"
)

// handoffDB ingests the Claude Code and Codex fixture sessions, which both
// ran in /home/dev/proj, and returns the database and that project's id.
func handoffDB(t *testing.T) (*sql.DB, int64) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, claudecode.New(), filepath.Join("..", "ingest", "claudecode", "testdata", "session.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, codex.New(), filepath.Join("..", "ingest", "codex", "testdata", "session.jsonl")); err != nil {
		t.Fatal(err)
	}
	projectID, found, err := store.LookupProject(db, "/home/dev/proj")
	if err != nil || !found {
		t.Fatalf("fixture project not found: %v", err)
	}
	return db, projectID
}

// A handoff written after a Claude Code session is what `kae resume` shows
// afterwards, from any agent: resume is per project, not per agent.
func TestHandoffThenResume(t *testing.T) {
	db, projectID := handoffDB(t)

	var saved bytes.Buffer
	if err := writeHandoff(&saved, db, projectID, "claude-code:sess-1"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"saved handoff as note 1",
		"Handoff: WAL mode question",
		"Handoff for the claude-code session claude-code:sess-1",
		"not summarised",
		// The objective is the first prompt, quoted.
		"Objective (the first prompt):\n  How do I open the sqlite database with WAL mode?",
		"Last exchanges:",
		`Full record: get_session(session_id="claude-code:sess-1", from_seq=`,
	} {
		if !strings.Contains(saved.String(), want) {
			t.Errorf("handoff lacks %q:\n%s", want, saved.String())
		}
	}

	// The note links to its source session and carries the handoff tag.
	var session, tags, agent string
	var anchor int
	if err := db.QueryRow(`SELECT session_id, anchor_seq, tags, agent FROM notes WHERE id = 1`).Scan(&session, &anchor, &tags, &agent); err != nil {
		t.Fatal(err)
	}
	if session != "claude-code:sess-1" || tags != `["handoff"]` || agent != handoff.Author {
		t.Errorf("note fields: session %q, tags %s, agent %q", session, tags, agent)
	}
	var inChunk int
	db.QueryRow(`SELECT count(*) FROM chunks WHERE session_id = ? AND first_seq = ?`, session, anchor).Scan(&inChunk)
	if inChunk != 1 {
		t.Errorf("anchor_seq %d is not the start of a stored exchange", anchor)
	}

	var resumed bytes.Buffer
	if err := printResume(&resumed, db, projectID); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Handoff: WAL mode question", "(note 1, ", "written by kae", "Objective (the first prompt):", `get_session(session_id="claude-code:sess-1"`} {
		if !strings.Contains(resumed.String(), want) {
			t.Errorf("resume lacks %q:\n%s", want, resumed.String())
		}
	}
	// The source call is given once, not repeated under the body.
	if n := strings.Count(resumed.String(), "get_session("); n != 1 {
		t.Errorf("the source call appears %d times:\n%s", n, resumed.String())
	}
}

// Without --session the latest session in the project is handed off, and
// the files it changed are listed.
func TestHandoffDefaultsToLatestSessionAndListsFiles(t *testing.T) {
	db, projectID := handoffDB(t)
	latest, err := handoff.LatestSession(db, projectID)
	if err != nil {
		t.Fatal(err)
	}
	// Give that session two changed files and one that was only read.
	for _, f := range []struct{ path, action string }{
		{"internal/store/store.go", "edit"}, {"internal/store/store.go", "edit"},
		{"internal/store/store_test.go", "create"}, {"README.md", "read"},
	} {
		if _, err := db.Exec(`INSERT INTO file_touches(session_id, project_id, path, action) VALUES (?, ?, ?, ?)`, latest, projectID, f.path, f.action); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE sessions SET git_branch = 'main', starting_commit = 'aaaaaaaaaaaa1111', ending_commit = 'bbbbbbbbbbbb2222', commit_source = 'branch' WHERE id = ?`, latest); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := writeHandoff(&out, db, projectID, ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"session " + latest,
		"Files changed:\n  - internal/store/store.go (edit, 2)\n  - internal/store/store_test.go (create, 1)",
		"Git: branch main, commits aaaaaaaaaa..bbbbbbbbbb\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("handoff lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "README.md") {
		t.Errorf("a file that was only read is listed as changed:\n%s", out.String())
	}
}

// Handing the same session off again replaces the earlier handoff, so
// resume shows one current note. A handoff an agent wrote is left alone.
func TestHandoffTwiceSupersedesTheFirst(t *testing.T) {
	db, projectID := handoffDB(t)
	if _, err := db.Exec(`INSERT INTO notes(project_id, session_id, agent, kind, title, tags) VALUES (?, 'claude-code:sess-1', 'claude-code', 'todo', 'agent handoff', '["handoff"]')`, projectID); err != nil {
		t.Fatal(err)
	}
	var first, second bytes.Buffer
	if err := writeHandoff(&first, db, projectID, "claude-code:sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := writeHandoff(&second, db, projectID, "claude-code:sess-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.String(), "saved handoff as note 3 (replaces note 2") {
		t.Fatalf("second handoff should replace the first:\n%s", second.String())
	}
	var agentNote, kaeFirst sql.NullInt64
	db.QueryRow(`SELECT superseded_by FROM notes WHERE id = 1`).Scan(&agentNote)
	db.QueryRow(`SELECT superseded_by FROM notes WHERE id = 2`).Scan(&kaeFirst)
	if agentNote.Valid {
		t.Error("the agent's own handoff was retired")
	}
	if !kaeFirst.Valid || kaeFirst.Int64 != 3 {
		t.Errorf("first kae handoff superseded_by = %v, want 3", kaeFirst)
	}
	var resumed bytes.Buffer
	if err := printResume(&resumed, db, projectID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resumed.String(), "(note 3, ") {
		t.Errorf("resume should show the newest handoff:\n%s", resumed.String())
	}
}

func TestHandoffRefusals(t *testing.T) {
	db, projectID := handoffDB(t)
	other, err := store.ResolveProject(db, "/home/dev/elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		project   int64
		session   string
		wantError string
	}{
		{"a session of another project", other, "claude-code:sess-1", "belongs to another project"},
		{"a session that does not exist", projectID, "claude-code:nope", "not found"},
		{"a project with no sessions", other, "", "no sessions to hand off"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := writeHandoff(&out, db, c.project, c.session)
			if err == nil || !strings.Contains(err.Error(), c.wantError) {
				t.Fatalf("want an error containing %q, got %v", c.wantError, err)
			}
		})
	}
	var notes int
	db.QueryRow(`SELECT count(*) FROM notes`).Scan(&notes)
	if notes != 0 {
		t.Fatalf("a refused handoff saved %d note(s)", notes)
	}

	// Resume with nothing saved explains itself and records nothing.
	var out bytes.Buffer
	if err := printResume(&out, db, projectID); err != nil || !strings.Contains(out.String(), "no handoff saved") {
		t.Fatalf("resume with no handoff: %q, %v", out.String(), err)
	}
}

// `kae resume` is a pickup, and `kae doctor` counts it, for a handoff that
// no checkpoint recorded.
func TestResumeCountsAsPickupInDoctor(t *testing.T) {
	db, projectID := handoffDB(t)
	var sink bytes.Buffer
	if err := writeHandoff(&sink, db, projectID, ""); err != nil {
		t.Fatal(err)
	}
	report := func() doctor.Report {
		r, err := doctor.Collect(context.Background(), db, projectID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := report(); r.HandoffsSaved != 1 || r.HandoffsLinked != 1 || r.HandoffsPickedUp != 0 {
		t.Fatalf("after kae handoff: saved %d, linked %d, picked up %d", r.HandoffsSaved, r.HandoffsLinked, r.HandoffsPickedUp)
	}
	if err := printResume(&sink, db, projectID); err != nil {
		t.Fatal(err)
	}
	if r := report(); r.HandoffsPickedUp != 1 {
		t.Fatalf("after kae resume: picked up %d, want 1", r.HandoffsPickedUp)
	}
}
