// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package firstrun

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/ingest/codex"
	"github.com/danPoku/kaectx/internal/store"
)

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

// ingestFixtures loads the two adapters' real-shaped fixture sessions.
func ingestFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := ingest.Run(db, claudecode.New(), filepath.Join("..", "ingest", "claudecode", "testdata", "session.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, codex.New(), filepath.Join("..", "ingest", "codex", "testdata", "session.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestQueryFromTitle(t *testing.T) {
	cases := []struct{ title, want string }{
		{"How do I open the sqlite database with WAL mode?", "open sqlite database"},
		{"fix the refresh-token race in auth.go", "fix refresh token"},
		{"The the THE with from", ""},
		{"export KEY=[REDACTED] then deploy staging", "export key deploy"},
		{"", ""},
		{"sqlite sqlite SQLite vectors", "sqlite vectors"},
	}
	for _, c := range cases {
		if got := queryFromTitle(c.title); got != c.want {
			t.Errorf("queryFromTitle(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

func TestCandidatesGoFromSpecificToGeneral(t *testing.T) {
	cases := []struct {
		query string
		want  []string
	}{
		{"", nil},
		{"wal", []string{"wal"}},
		{"wal mode", []string{"wal mode", "wal", "mode"}},
		{"wal mode question", []string{"wal mode question", "wal mode", "mode question", "wal question", "wal", "mode", "question"}},
	}
	for _, c := range cases {
		got := candidates(c.query)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("candidates(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}

func TestLoadCountsWhatWasIngested(t *testing.T) {
	db := openDB(t)
	if n, err := CountSessions(db); err != nil || n != 0 {
		t.Fatalf("a new database should have no sessions: %d, %v", n, err)
	}
	ingestFixtures(t, db)

	o, err := Load(db)
	if err != nil {
		t.Fatal(err)
	}
	// Every number is checked against a direct query on the same database.
	var sessions, projects, redactions int
	var oldest, newest string
	if err := db.QueryRow(`SELECT count(*), count(DISTINCT project_id), min(started_at), max(started_at) FROM sessions`).
		Scan(&sessions, &projects, &oldest, &newest); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT content FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		redactions += strings.Count(c, "[REDACTED]")
	}
	rows.Close()

	if sessions < 2 {
		t.Fatalf("fixtures should give at least two sessions, got %d", sessions)
	}
	if o.Sessions != sessions || o.Projects != projects || o.Oldest != oldest || o.Newest != newest || o.Redactions != redactions {
		t.Fatalf("overview %+v does not match the database (%d sessions, %d projects, %s..%s, %d redactions)",
			o, sessions, projects, oldest, newest, redactions)
	}
	total := 0
	agents := map[string]bool{}
	for _, c := range o.ByAgent {
		total += c.Sessions
		agents[c.Name] = true
	}
	if total != sessions || !agents["claude-code"] || !agents["codex"] {
		t.Fatalf("per-agent counts wrong: %+v", o.ByAgent)
	}
}

func TestLoadCountsRedactions(t *testing.T) {
	db := openDB(t)
	ingestFixtures(t, db)
	before, err := Load(db)
	if err != nil {
		t.Fatal(err)
	}
	// Three placeholders in one stored message must count as three.
	if _, err := db.Exec(`UPDATE messages SET content = content || ' a=[REDACTED] b=[REDACTED] [REDACTED]'
		WHERE rowid = (SELECT min(rowid) FROM messages)`); err != nil {
		t.Fatal(err)
	}
	after, err := Load(db)
	if err != nil {
		t.Fatal(err)
	}
	if after.Redactions != before.Redactions+3 {
		t.Fatalf("redactions went from %d to %d, want +3", before.Redactions, after.Redactions)
	}
}

// The suggested search must find something whenever the project has
// sessions, and fall back to the whole catalogue when it has none.
func TestSuggestAlwaysReturnsAHit(t *testing.T) {
	db := openDB(t)
	ingestFixtures(t, db)

	var projectID int64
	if err := db.QueryRow(`SELECT project_id FROM sessions WHERE agent = 'claude-code' AND parent_session_id IS NULL LIMIT 1`).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	s, ok, err := Suggest(context.Background(), db, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || s.Query == "" || s.Hit.SessionID == "" || s.AllProjects {
		t.Fatalf("want a suggestion from the project itself, got %+v ok=%v", s, ok)
	}
	if s.Hit.ProjectID != projectID {
		t.Fatalf("hit is from project %d, want %d", s.Hit.ProjectID, projectID)
	}

	// A project with no sessions: the suggestion comes from elsewhere and
	// says the search needs --all-projects.
	res, err := db.Exec(`INSERT INTO projects (name) VALUES ('empty')`)
	if err != nil {
		t.Fatal(err)
	}
	empty, _ := res.LastInsertId()
	s, ok, err = Suggest(context.Background(), db, empty)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !s.AllProjects {
		t.Fatalf("want an all-projects suggestion, got %+v ok=%v", s, ok)
	}
	if out := Format(Overview{Sessions: 1, Projects: 1}, &s); !strings.Contains(out, "--all-projects") {
		t.Fatalf("the command shown must carry --all-projects:\n%s", out)
	}

	// An empty catalogue has nothing to suggest.
	if _, ok, err := Suggest(context.Background(), openDB(t), 1); err != nil || ok {
		t.Fatalf("empty database: ok=%v err=%v", ok, err)
	}
}

func TestFormat(t *testing.T) {
	o := Overview{
		Sessions: 412, Projects: 9, Oldest: "2026-02-03T08:00:00Z", Newest: "2026-11-23T21:00:00Z", Redactions: 57,
		ByAgent:     []Count{{"claude-code", 301}, {"codex", 111}},
		TopProjects: []Count{{"api", 120}, {"web", 1}},
	}
	out := Format(o, nil)
	for _, want := range []string{
		"kae found 412 sessions across 9 projects, from 2026-02-03 to 2026-11-23.",
		"claude-code  301 sessions", "codex        111 sessions", "web          1 session\n",
		"Secrets redacted before storing: 57",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Try:") {
		t.Errorf("no suggestion was given, but one is shown:\n%s", out)
	}
}
