// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package repair

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/danPoku/kaectx/internal/store"
)

type projectsFixture struct {
	db           *sql.DB
	parent, repo int64
}

// misfiledDB builds a database as subagent-first ingest left it: a session
// whose subagent ran in the parent folder sits in the parent's project and
// starts at the subagent's first line, although its main transcript starts
// earlier, in the repo.
func misfiledDB(t *testing.T) projectsFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	f := projectsFixture{db: db}
	for _, p := range []struct {
		id   *int64
		name string
		path string
	}{{&f.parent, "work", "/work"}, {&f.repo, "repo", "/work/repo"}} {
		res, err := db.Exec(`INSERT INTO projects(name) VALUES (?)`, p.name)
		if err != nil {
			t.Fatal(err)
		}
		*p.id, _ = res.LastInsertId()
		exec1(t, db, `INSERT INTO project_paths(path, project_id) VALUES (?, ?)`, filepath.Clean(p.path), *p.id)
	}

	session := func(id, agent string, project int64, startedAt string) {
		exec1(t, db, `INSERT INTO sessions(id, agent, native_id, project_id, started_at) VALUES (?, ?, ?, ?, ?)`,
			id, agent, id, project, startedAt)
	}
	seq := map[string]int{}
	message := func(session, cwd, at string, sidechain bool) {
		raw := fmt.Sprintf(`{"type":"user","cwd":%q,"isSidechain":%t,"timestamp":%q}`, cwd, sidechain, at)
		exec1(t, db, `INSERT INTO messages(session_id, seq, role, content, created_at, raw) VALUES (?, ?, 'user', 'x', ?, ?)`,
			session, seq[session], at, raw)
		seq[session]++
	}

	// The subagent's line was stored first (seq 0), but the main
	// transcript's first line is earlier and in the repo.
	session("claude-code:misfiled", "claude-code", f.parent, "2026-09-20T10:00:30.000Z")
	message("claude-code:misfiled", "/work", "2026-09-20T10:00:30.000Z", true)
	message("claude-code:misfiled", "/work/repo", "2026-09-20T10:00:00.100Z", false)
	message("claude-code:misfiled", "/work", "2026-09-20T10:01:00.000Z", false)
	exec1(t, db, `INSERT INTO file_touches(session_id, message_id, project_id, path, action)
	              SELECT session_id, id, ?, 'main.go', 'edit' FROM messages WHERE session_id = 'claude-code:misfiled' AND seq = 2`, f.parent)

	// Already right: starts in the repo, drifts to the parent.
	session("claude-code:right", "claude-code", f.repo, "2026-09-20T09:00:00Z")
	message("claude-code:right", "/work/repo", "2026-09-20T09:00:00Z", false)
	message("claude-code:right", "/work", "2026-09-20T09:05:00Z", false)
	exec1(t, db, `UPDATE sessions SET meta = '{"provisional_project":true}' WHERE id = 'claude-code:right'`)

	// Only subagent lines stored: nothing trustworthy to re-file from.
	session("claude-code:orphan", "claude-code", f.parent, "2026-09-20T08:00:00Z")
	message("claude-code:orphan", "/work/repo", "2026-09-20T08:00:00Z", true)

	// Created by an ai-title line read first: no project, no start.
	exec1(t, db, `INSERT INTO sessions(id, agent, native_id, started_at) VALUES ('claude-code:untimed', 'claude-code', 'untimed', '')`)
	message("claude-code:untimed", "/work/repo", "2026-09-20T11:00:00Z", false)

	// Starts in a folder no project owns (a WSL path read on Windows looks
	// like this): left alone rather than given a new project.
	session("claude-code:elsewhere", "claude-code", f.parent, "2026-09-20T08:30:00Z")
	message("claude-code:elsewhere", "/other/place", "2026-09-20T08:30:00Z", false)

	// A Codex session that logged no cwd: nothing to judge it by.
	session("codex:other", "codex", f.parent, "2026-09-20T07:00:00Z")
	message("codex:other", "/work/repo", "2026-09-20T07:00:00Z", false)
	return f
}

func (f projectsFixture) session(t *testing.T, id string) (project int64, startedAt string) {
	t.Helper()
	if err := f.db.QueryRow(`SELECT project_id, started_at FROM sessions WHERE id = ?`, id).Scan(&project, &startedAt); err != nil {
		t.Fatalf("session %s: %v", id, err)
	}
	return project, startedAt
}

func TestProjectsRefilesSessionsByMainTranscript(t *testing.T) {
	f := misfiledDB(t)

	res, err := Projects(f.db, false)
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if res.Examined != 4 || res.NoMain != 1 || res.NoCWD != 1 || res.Unknown != 1 || res.Restarted != 2 {
		t.Errorf("result = %+v, want Examined 4, NoMain 1, NoCWD 1, Unknown 1, Restarted 2", res)
	}
	var projects int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&projects); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if projects != 2 {
		t.Errorf("projects = %d, want 2: a repair must not create projects", projects)
	}
	repo := fmt.Sprintf("repo (%d)", f.repo)
	want := []Move{
		{SessionID: "claude-code:misfiled", From: fmt.Sprintf("work (%d)", f.parent), To: repo},
		{SessionID: "claude-code:untimed", To: repo},
	}
	if len(res.Moved) != len(want) || res.Moved[0] != want[0] || res.Moved[1] != want[1] {
		t.Errorf("Moved = %+v, want %+v", res.Moved, want)
	}

	tests := []struct {
		id          string
		wantProject int64
		wantStart   string
	}{
		{"claude-code:misfiled", f.repo, "2026-09-20T10:00:00.100Z"},
		{"claude-code:right", f.repo, "2026-09-20T09:00:00Z"},
		{"claude-code:untimed", f.repo, "2026-09-20T11:00:00Z"},
		{"claude-code:orphan", f.parent, "2026-09-20T08:00:00Z"},
		{"claude-code:elsewhere", f.parent, "2026-09-20T08:30:00Z"},
		{"codex:other", f.parent, "2026-09-20T07:00:00Z"},
	}
	for _, tt := range tests {
		project, start := f.session(t, tt.id)
		if project != tt.wantProject || start != tt.wantStart {
			t.Errorf("%s = project %d, started %s; want project %d, started %s", tt.id, project, start, tt.wantProject, tt.wantStart)
		}
	}

	var provisional int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE json_extract(meta, '$.provisional_project') IS NOT NULL
	                         AND id IN ('claude-code:right', 'claude-code:misfiled')`).Scan(&provisional); err != nil {
		t.Fatalf("provisional markers: %v", err)
	}
	if provisional != 0 {
		t.Errorf("%d re-filed sessions keep a provisional marker, want none: the next ingested line could move them", provisional)
	}

	var touchProject int64
	if err := f.db.QueryRow(`SELECT project_id FROM file_touches WHERE session_id = 'claude-code:misfiled'`).Scan(&touchProject); err != nil {
		t.Fatalf("file_touches: %v", err)
	}
	if touchProject != f.repo {
		t.Errorf("file_touches.project_id = %d, want it moved with the session to %d", touchProject, f.repo)
	}

	again, err := Projects(f.db, false)
	if err != nil {
		t.Fatalf("second Projects: %v", err)
	}
	if len(again.Moved) != 0 || again.Restarted != 0 {
		t.Errorf("second run = %+v, want nothing moved or restarted", again)
	}
}

// TestProjectsMergesSpellingsOfOneFolder: before lookup ignored path
// spelling, one Windows folder could become two projects (C:\proj from a
// Windows agent, /mnt/c/proj from a WSL one). Lookup now resolves both to the
// lower id, and the repair moves the other project's sessions there, for
// Codex by its logged cwd and for Claude Code by its main transcript.
func TestProjectsMergesSpellingsOfOneFolder(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, p := range []string{`C:\proj`, "/mnt/c/proj"} {
		res, err := db.Exec(`INSERT INTO projects(name) VALUES ('proj')`)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		exec1(t, db, `INSERT INTO project_paths(path, project_id) VALUES (?, ?)`, p, id)
		ids = append(ids, id)
	}
	kept, split := ids[0], ids[1]
	exec1(t, db, `INSERT INTO sessions(id, agent, native_id, project_id, cwd, started_at)
	              VALUES ('codex:wsl', 'codex', 'wsl', ?, '/mnt/c/proj/api', '2026-09-20T07:00:00Z')`, split)
	exec1(t, db, `INSERT INTO sessions(id, agent, native_id, project_id, started_at)
	              VALUES ('claude-code:wsl', 'claude-code', 'wsl', ?, '2026-09-20T08:00:00Z')`, split)
	exec1(t, db, `INSERT INTO messages(session_id, seq, role, content, created_at, raw)
	              VALUES ('claude-code:wsl', 0, 'user', 'x', '2026-09-20T08:00:00Z', '{"cwd":"/mnt/c/proj","isSidechain":false}')`)

	res, err := Projects(db, false)
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(res.Moved) != 2 {
		t.Errorf("Moved = %+v, want both sessions", res.Moved)
	}
	for _, id := range []string{"codex:wsl", "claude-code:wsl"} {
		var project int64
		if err := db.QueryRow(`SELECT project_id FROM sessions WHERE id = ?`, id).Scan(&project); err != nil {
			t.Fatal(err)
		}
		if project != kept {
			t.Errorf("%s in project %d, want %d", id, project, kept)
		}
	}
	if again, err := Projects(db, false); err != nil || len(again.Moved) != 0 {
		t.Errorf("second run = %+v, %v; want nothing moved", again, err)
	}
}

func TestProjectsDryRunReportsButWritesNothing(t *testing.T) {
	f := misfiledDB(t)

	res, err := Projects(f.db, true)
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(res.Moved) != 2 {
		t.Errorf("Moved = %+v, want the misfiled and untimed sessions reported", res.Moved)
	}
	if project, start := f.session(t, "claude-code:misfiled"); project != f.parent || start != "2026-09-20T10:00:30.000Z" {
		t.Errorf("after dry run: project %d, started %s; want it unchanged", project, start)
	}
}
