// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package repair

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/explain"
	"github.com/danPoku/kaectx/internal/store"
)

func gitRepo(t *testing.T, dates ...string) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	run := func(date string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("", "init", "-q", "-b", "main")
	var hashes []string
	for _, d := range dates {
		run(d, "commit", "-q", "--allow-empty", "-m", "c "+d)
		hashes = append(hashes, run("", "rev-parse", "HEAD"))
	}
	return dir, hashes
}

func exec1(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

type fixture struct {
	db      *sql.DB
	project int64
	repo    string
	commits []string
}

// legacyDB builds a database as v0.1.3 or the first file-aware-memory draft would have
// left it: commits stamped with HEAD, paths relative to the session cwd, and
// every file_touch un-rooted.
func legacyDB(t *testing.T) fixture {
	t.Helper()
	repo, c := gitRepo(t, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z")
	pkg := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO projects(name) VALUES ('p')`)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := res.LastInsertId()

	sess := func(id string, cwd any, started string, start, end any) {
		exec1(t, db, `INSERT INTO sessions(id, agent, native_id, project_id, cwd, started_at, starting_commit, ending_commit)
		              VALUES (?, 'codex', ?, ?, ?, ?, ?, ?)`, id, id, p, cwd, started, start, end)
	}
	msg := func(id string, seq int, at string) {
		exec1(t, db, `INSERT INTO messages(session_id, seq, role, kind, content, created_at) VALUES (?, ?, 'user', 'text', 'm', ?)`, id, seq, at)
	}
	touch := func(id, path string, rooted int) {
		exec1(t, db, `INSERT INTO file_touches(session_id, project_id, path, action, rooted) VALUES (?, ?, ?, 'edit', ?)`, id, p, path, rooted)
	}

	// A: launched in a subdirectory, wrongly stamped with HEAD (c3) at ingest.
	sess("A", pkg, "2026-01-10T00:00:00Z", c[2], c[2])
	exec1(t, db, `UPDATE sessions SET git_branch = 'main' WHERE id = 'A'`)
	msg("A", 0, "2026-01-10T00:00:00Z")
	msg("A", 1, "2026-02-10T00:00:00Z")
	touch("A", "a.go", 0)
	touch("A", "/etc/outside.conf", 0)
	touch("A", filepath.Join(pkg, "b.go"), 0)
	touch("A", "../c.go", 0)
	// B: launched at the root, no commits recorded.
	sess("B", repo, "2026-03-15T00:00:00Z", nil, nil)
	touch("B", "z.go", 0)
	// C: its checkout no longer exists.
	sess("C", "/gone/checkout", "2026-01-10T00:00:00Z", "keep-start", "keep-end")
	touch("C", "q.go", 0)
	// D: no cwd at all.
	sess("D", nil, "2026-01-10T00:00:00Z", nil, nil)
	touch("D", "n.go", 0)
	// F: a directory that exists but is not a repository (like a home dir).
	sess("F", t.TempDir(), "2026-01-10T00:00:00Z", nil, nil)
	touch("F", "f.go", 0)
	// E: already-rooted row that must never be prefixed a second time.
	sess("E", pkg, "2026-02-10T00:00:00Z", nil, nil)
	touch("E", "pkg/e.go", 1)

	return fixture{db, p, repo, c}
}

func (f fixture) paths(t *testing.T, session string) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT path FROM file_touches WHERE session_id = ? ORDER BY id`, session)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		out = append(out, p)
	}
	return out
}

func (f fixture) span(t *testing.T, session string) (start, end string) {
	t.Helper()
	var s, e sql.NullString
	if err := f.db.QueryRow(`SELECT starting_commit, ending_commit FROM sessions WHERE id = ?`, session).Scan(&s, &e); err != nil {
		t.Fatal(err)
	}
	return s.String, e.String
}

func (f fixture) source(t *testing.T, session string) string {
	t.Helper()
	var s sql.NullString
	if err := f.db.QueryRow(`SELECT commit_source FROM sessions WHERE id = ?`, session).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s.String
}

func (f fixture) unrooted(t *testing.T) int {
	t.Helper()
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM file_touches WHERE rooted = 0`).Scan(&n)
	return n
}

func TestGitRepairsCommitsAndPaths(t *testing.T) {
	f := legacyDB(t)
	c := f.commits

	res, err := Git(f.db, store.NewGitResolver(), false)
	if err != nil {
		t.Fatalf("Git: %v", err)
	}
	want := Result{
		Sessions: 5, CommitsUpdated: 3, CommitsUnresolved: 2, CommitsUnverified: 1,
		TouchesRewritten: 3, TouchesRooted: 7, TouchesPending: 1,
	}
	if res != want {
		t.Errorf("result = %+v\nwant     %+v", res, want)
	}

	// Commits: A had HEAD stamped on it; it must now be the span it lived in.
	if s, e := f.span(t, "A"); s != c[0] || e != c[1] {
		t.Errorf("A span = %q..%q, want %q..%q", s, e, c[0], c[1])
	}
	if s, e := f.span(t, "B"); s != c[2] || e != c[2] {
		t.Errorf("B span = %q..%q, want the commit current at its start", s, e)
	}
	if s, e := f.span(t, "E"); s != c[1] || e != c[1] {
		t.Errorf("E span = %q..%q, want %q", s, e, c[1])
	}
	// An unverifiable value must be kept, not blanked.
	if s, e := f.span(t, "C"); s != "keep-start" || e != "keep-end" {
		t.Errorf("C span = %q..%q, want the old values kept", s, e)
	}

	// How much to trust each span: A logged its branch, B and E did not (so
	// their commits were read from HEAD's history), C's repo is gone.
	for id, want := range map[string]string{"A": "branch", "B": "head", "E": "head", "C": "unverified", "D": "", "F": ""} {
		if got := f.source(t, id); got != want {
			t.Errorf("session %s commit_source = %q, want %q", id, got, want)
		}
	}

	// Paths.
	if got := strings.Join(f.paths(t, "A"), ","); got != "pkg/a.go,/etc/outside.conf,pkg/b.go,c.go" {
		t.Errorf("A paths = %s", got)
	}
	if got := f.paths(t, "B"); len(got) != 1 || got[0] != "z.go" {
		t.Errorf("B paths = %v, want unchanged", got)
	}
	if got := f.paths(t, "D"); got[0] != "n.go" {
		t.Errorf("D paths = %v, want unchanged", got)
	}
	if got := f.paths(t, "E"); got[0] != "pkg/e.go" {
		t.Errorf("E (already rooted) = %v, must not gain a second prefix", got)
	}
	// A directory that exists but is not a repo is settled (cwd-relative is
	// final there); only the directory that is gone stays pending.
	if got := f.paths(t, "F"); got[0] != "f.go" {
		t.Errorf("F paths = %v, want unchanged", got)
	}
	if n := f.unrooted(t); n != 1 {
		t.Errorf("un-rooted rows = %d, want 1 (session C, whose directory is gone)", n)
	}

	// The point of it all: explain from the repo root now finds the edit.
	out, err := explain.File(f.db, f.project, "pkg/a.go", 10)
	if err != nil || len(out.Sessions) != 1 || out.Sessions[0].SessionID != "A" {
		t.Errorf("explain pkg/a.go = %+v, %v; want session A", out.Sessions, err)
	}
}

// Running it again must change nothing — in particular it must not add the
// subdirectory prefix a second time.
func TestGitRepairIsIdempotent(t *testing.T) {
	f := legacyDB(t)
	if _, err := Git(f.db, store.NewGitResolver(), false); err != nil {
		t.Fatal(err)
	}
	before := strings.Join(f.paths(t, "A"), ",")

	res, err := Git(f.db, store.NewGitResolver(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.CommitsUpdated != 0 || res.CommitsUnverified != 0 || res.TouchesRewritten != 0 || res.TouchesRooted != 0 {
		t.Errorf("second run changed things: %+v", res)
	}
	if res.TouchesPending != 1 || res.CommitsUnresolved != 2 {
		t.Errorf("second run pending/unresolved = %d/%d, want 1/2", res.TouchesPending, res.CommitsUnresolved)
	}
	if after := strings.Join(f.paths(t, "A"), ","); after != before {
		t.Errorf("paths changed on re-run: %s -> %s", before, after)
	}
}

func TestGitRepairDryRunReportsButWritesNothing(t *testing.T) {
	f := legacyDB(t)
	c := f.commits
	res, err := Git(f.db, store.NewGitResolver(), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.CommitsUpdated != 3 || res.TouchesRewritten != 3 {
		t.Errorf("dry-run result = %+v, want it to report the pending changes", res)
	}
	if s, e := f.span(t, "A"); s != c[2] || e != c[2] {
		t.Errorf("dry run modified commits: %q..%q", s, e)
	}
	if got := f.paths(t, "A")[0]; got != "a.go" {
		t.Errorf("dry run modified paths: %q", got)
	}
	if n := f.unrooted(t); n != 8 {
		t.Errorf("un-rooted rows = %d, want all 8 still pending", n)
	}
}

// A repair that fails part-way must leave the database exactly as it was.
func TestGitRepairRollsBackOnError(t *testing.T) {
	f := legacyDB(t)
	// Make the path update fail after the commit updates have already run.
	exec1(t, f.db, `CREATE TRIGGER boom BEFORE UPDATE OF path ON file_touches BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	if _, err := Git(f.db, store.NewGitResolver(), false); err == nil {
		t.Fatal("expected the trigger to fail the repair")
	}
	if s, _ := f.span(t, "A"); s != f.commits[2] {
		t.Errorf("commit update leaked out of a failed repair: %q", s)
	}
}

// Sessions ingested before kae read apply_patch have no file records; the
// patch text is still in messages.content and must be enough to rebuild them.
func TestTouchesBackfillsCodexApplyPatchFromStoredMessages(t *testing.T) {
	f := legacyDB(t)
	pkg := filepath.Join(f.repo, "pkg")
	nonGit := t.TempDir()

	sess := func(id string, cwd any) {
		exec1(t, f.db, `INSERT INTO sessions(id, agent, native_id, project_id, cwd, started_at) VALUES (?, 'codex', ?, ?, ?, '2026-02-01T00:00:00Z')`, id, id, f.project, cwd)
	}
	call := func(session string, seq int, tool, content string) int64 {
		res, err := f.db.Exec(`INSERT INTO messages(session_id, seq, role, kind, tool_name, content, created_at)
		                       VALUES (?, ?, 'assistant', 'tool_call', ?, ?, '2026-02-01T00:00:0'||?||'Z')`, session, seq, tool, content, seq)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	sess("X", pkg)
	call("X", 1, "apply_patch", "*** Begin Patch\n*** Update File: "+filepath.Join(pkg, "x.go")+"\n@@\n-a\n+b\n"+
		"*** Add File: y.go\n+z\n*** Delete File: ../z.go\n*** Update File: m.go\n*** Move to: n.go\n*** End Patch")
	call("X", 2, "exec", "*** Update File: nope.go") // a shell call, never a touch
	call("X", 3, "apply_patch", "*** Begin Patch\n*** End Patch")
	dup := call("X", 4, "apply_patch", "*** Update File: dup.go")
	exec1(t, f.db, `INSERT INTO file_touches(session_id, message_id, project_id, path, action, rooted) VALUES ('X', ?, ?, 'dup.go', 'edit', 1)`, dup, f.project)
	sess("G", "/gone/dir")
	call("G", 1, "apply_patch", "*** Update File: /gone/dir/g.go")
	sess("H", nonGit)
	call("H", 1, "apply_patch", "*** Update File: h.go")
	sess("W", nil)
	call("W", 1, "apply_patch", "*** Update File: /abs/w.go")

	var before int
	f.db.QueryRow(`SELECT COUNT(*) FROM file_touches`).Scan(&before)

	// Dry run reports but writes nothing.
	dry, err := Touches(f.db, store.NewGitResolver(), true)
	if err != nil {
		t.Fatal(err)
	}
	var after int
	f.db.QueryRow(`SELECT COUNT(*) FROM file_touches`).Scan(&after)
	if after != before || dry.Added != 8 {
		t.Fatalf("dry run: added=%d rows %d->%d, want it to report 8 and write none", dry.Added, before, after)
	}

	res, err := Touches(f.db, store.NewGitResolver(), false)
	if err != nil {
		t.Fatal(err)
	}
	if want := (TouchesResult{Messages: 5, Added: 8, Empty: 1, Pending: 1}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}

	if got := strings.Join(f.paths(t, "X"), ","); got != "dup.go,pkg/x.go,pkg/y.go,z.go,pkg/m.go,pkg/n.go" {
		t.Errorf("X paths = %s", got)
	}
	rows, _ := f.db.Query(`SELECT action FROM file_touches WHERE session_id = 'X' AND path <> 'dup.go' ORDER BY id`)
	var actions []string
	for rows.Next() {
		var a string
		rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	if strings.Join(actions, ",") != "edit,create,delete,edit,rename" {
		t.Errorf("X actions = %v", actions)
	}
	if got := f.paths(t, "G"); got[0] != "g.go" {
		t.Errorf("G paths = %v", got)
	}
	if got := f.paths(t, "H"); got[0] != "h.go" {
		t.Errorf("H paths = %v", got)
	}
	if got := f.paths(t, "W"); got[0] != "/abs/w.go" {
		t.Errorf("W paths = %v, want the absolute path untouched", got)
	}

	// Row details: linked to its message and project, timestamped, rooted
	// except where the directory is gone.
	var msgID, project sql.NullInt64
	var created sql.NullString
	var rooted int
	if err := f.db.QueryRow(`SELECT message_id, project_id, created_at, rooted FROM file_touches WHERE path = 'pkg/x.go'`).Scan(&msgID, &project, &created, &rooted); err != nil {
		t.Fatal(err)
	}
	if !msgID.Valid || project.Int64 != f.project || created.String != "2026-02-01T00:00:01Z" || rooted != 1 {
		t.Errorf("pkg/x.go row = msg %v project %v created %q rooted %d", msgID, project, created.String, rooted)
	}
	f.db.QueryRow(`SELECT rooted FROM file_touches WHERE path = 'g.go'`).Scan(&rooted)
	if rooted != 0 {
		t.Errorf("g.go rooted = %d, want 0 (its directory is gone)", rooted)
	}

	// Idempotent: nothing more to add, and no duplicates.
	again, err := Touches(f.db, store.NewGitResolver(), false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Added != 0 {
		t.Errorf("second run added %d rows", again.Added)
	}
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM file_touches`).Scan(&n)
	if n != before+8 {
		t.Errorf("file_touches = %d, want %d", n, before+8)
	}

	// The end goal: explain can now answer for a Codex edit.
	out, err := explain.File(f.db, f.project, "pkg/x.go", 10)
	if err != nil || len(out.Sessions) != 1 || out.Sessions[0].SessionID != "X" {
		t.Errorf("explain pkg/x.go = %+v, %v; want session X", out.Sessions, err)
	}
}

// A session that began before the repository's first commit can resolve its
// end but not its start. A stored start value we can't re-derive is kept but
// downgrades the span to "unverified"; with nothing stored there is nothing
// to distrust, so the span keeps the source of the end that did resolve.
func TestGitRepairWhenOnlyOneEndOfASpanResolves(t *testing.T) {
	f := legacyDB(t)
	pkg := filepath.Join(f.repo, "pkg")
	for id, oldStart := range map[string]any{"P1": "old-start", "P2": nil} {
		exec1(t, f.db, `INSERT INTO sessions(id, agent, native_id, project_id, cwd, git_branch, started_at, starting_commit, ending_commit)
		                VALUES (?, 'codex', ?, ?, ?, 'main', '2025-12-01T00:00:00Z', ?, ?)`, id, id, f.project, pkg, oldStart, f.commits[2])
		exec1(t, f.db, `INSERT INTO messages(session_id, seq, role, kind, content, created_at) VALUES (?, 0, 'user', 'text', 'm', '2025-12-01T00:00:00Z')`, id)
		exec1(t, f.db, `INSERT INTO messages(session_id, seq, role, kind, content, created_at) VALUES (?, 1, 'user', 'text', 'm', '2026-02-10T00:00:00Z')`, id)
	}
	if _, err := Git(f.db, store.NewGitResolver(), false); err != nil {
		t.Fatal(err)
	}

	if s, e := f.span(t, "P1"); s != "old-start" || e != f.commits[1] {
		t.Errorf("P1 span = %q..%q, want the unverifiable start kept and the end re-derived", s, e)
	}
	if got := f.source(t, "P1"); got != "unverified" {
		t.Errorf("P1 source = %q, want unverified", got)
	}
	if s, e := f.span(t, "P2"); s != "" || e != f.commits[1] {
		t.Errorf("P2 span = %q..%q, want no start and a re-derived end", s, e)
	}
	if got := f.source(t, "P2"); got != "branch" {
		t.Errorf("P2 source = %q, want branch (nothing stored to distrust)", got)
	}
}
