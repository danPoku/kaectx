// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package sharedmem

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/redact"
	"github.com/danPoku/kaectx/internal/search"
	"github.com/danPoku/kaectx/internal/store"
)

// machine is one person's catalogue with one project in it.
type machine struct {
	db      *sql.DB
	project int64
}

func newMachine(t *testing.T) machine {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO projects(name, git_remote) VALUES ('api', 'github.com/acme/api')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return machine{db: db, project: id}
}

func (m machine) note(t *testing.T, kind, title, body, tags string) int64 {
	t.Helper()
	if tags == "" {
		tags = "[]"
	}
	res, err := m.db.Exec(`INSERT INTO notes(project_id, agent, kind, title, body, tags, created_at) VALUES (?, 'claude-code', ?, ?, ?, ?, '2026-11-23T10:00:00Z')`,
		m.project, kind, title, body, tags)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// found runs a note search the way search_notes does and returns each hit
// as "origin: title".
func (m machine) found(t *testing.T, query string) []string {
	t.Helper()
	hits, err := search.Notes(m.db, query, m.project, 20)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, h := range hits {
		out = append(out, h.Origin+": "+h.Title)
	}
	return out
}

func files(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, Dir))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// The journey the feature exists for: a note exported on one machine,
// carried by the repository, is returned by a note search on another.
func TestExportedNoteIsFoundOnAnotherMachine(t *testing.T) {
	laptop, server := newMachine(t), newMachine(t)
	repo := t.TempDir() // stands in for the checkout both machines have

	id := laptop.note(t, "decision", "Use pgx over database/sql", "We need LISTEN/NOTIFY, which database/sql does not expose.", `["db"]`)
	todo := laptop.note(t, "todo", "Ring the vendor", "", "")

	got, err := Export(laptop.db, laptop.project, repo, []int64{id}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Written) != 1 || !strings.HasPrefix(filepath.Base(got.Written[0]), "decision-use-pgx-over-database-sql-") {
		t.Fatalf("unexpected export: %+v", got)
	}
	content, err := os.ReadFile(filepath.Join(repo, got.Written[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"---\nkae: 1\nid: ", "kind: decision\n", `title: "Use pgx over database/sql"`, "date: 2026-11-23\n", `tags: ["db"]`, "\n---\n\nWe need LISTEN/NOTIFY"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("file lacks %q:\n%s", want, content)
		}
	}
	// A todo is not something to circulate, even when asked by id.
	if _, err := Export(laptop.db, laptop.project, repo, []int64{todo}, false); err == nil {
		t.Error("exporting a todo should be refused")
	}

	// "git pull" on the server, then ingest.
	imported, err := Import(server.db, server.project, []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Added != 1 || len(imported.Skipped) != 0 {
		t.Fatalf("server import: %+v", imported)
	}
	if hits := server.found(t, "pgx"); len(hits) != 1 || hits[0] != "repo: Use pgx over database/sql" {
		t.Fatalf("server search: %v", hits)
	}

	// On the laptop the file is its own note: importing must not make a
	// second copy, and the note there is still marked local.
	imported, err = Import(laptop.db, laptop.project, []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Added != 0 {
		t.Fatalf("the exporting machine imported its own note: %+v", imported)
	}
	if hits := laptop.found(t, "pgx"); len(hits) != 1 || hits[0] != "local: Use pgx over database/sql" {
		t.Fatalf("laptop search: %v", hits)
	}

	// Importing again changes nothing.
	if again, _ := Import(server.db, server.project, []string{repo}); again.Added+again.Updated+again.Removed != 0 {
		t.Fatalf("second import changed something: %+v", again)
	}
}

// Editing or deleting the file in the repository is reflected on the next
// ingest.
func TestEditsAndDeletionsFollowTheFile(t *testing.T) {
	laptop, server := newMachine(t), newMachine(t)
	repo := t.TempDir()
	id := laptop.note(t, "convention", "Table-driven tests", "Every adapter gets fixtures.", "")
	got, err := Export(laptop.db, laptop.project, repo, []int64{id}, false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, got.Written[0])
	if _, err := Import(server.db, server.project, []string{repo}); err != nil {
		t.Fatal(err)
	}

	// A reviewer edits the file in a pull request.
	content, _ := os.ReadFile(path)
	edited := strings.Replace(string(content), "Every adapter gets fixtures.", "Every adapter gets fixtures from real, redacted logs.", 1)
	if err := os.WriteFile(path, []byte(edited), 0644); err != nil {
		t.Fatal(err)
	}
	imported, err := Import(server.db, server.project, []string{repo})
	if err != nil || imported.Updated != 1 {
		t.Fatalf("after the edit: %+v, %v", imported, err)
	}
	var body string
	server.db.QueryRow(`SELECT body FROM notes WHERE origin = 'repo'`).Scan(&body)
	if body != "Every adapter gets fixtures from real, redacted logs." {
		t.Fatalf("body after the edit: %q", body)
	}
	if hits := server.found(t, "redacted"); len(hits) != 1 {
		t.Fatalf("the edit did not reach the search index: %v", hits)
	}

	// The file is deleted from the repository.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	imported, err = Import(server.db, server.project, []string{repo})
	if err != nil || imported.Removed != 1 {
		t.Fatalf("after the deletion: %+v, %v", imported, err)
	}
	if hits := server.found(t, "fixtures"); len(hits) != 0 {
		t.Fatalf("a note whose file is gone is still found: %v", hits)
	}
	// Deleting the file does not delete the author's own note.
	if hits := laptop.found(t, "fixtures"); len(hits) != 1 {
		t.Fatalf("the author's local note was affected: %v", hits)
	}
}

// Export never writes text the redaction rules match, in the title, the body
// or a tag.
func TestExportRedacts(t *testing.T) {
	m := newMachine(t)
	repo := t.TempDir()
	const awsKey = "AKIAIOSFODNN7EXAMPLE"
	const ghToken = "ghp_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"
	m.note(t, "gotcha", "Staging key is "+awsKey, "Set GITHUB_TOKEN="+ghToken+" before running the deploy script.", `["`+awsKey+`"]`)
	m.note(t, "fact", "Deploys run from main", "No secrets here.", "")

	if _, err := Export(m.db, m.project, repo, nil, true); err != nil {
		t.Fatal(err)
	}
	names := files(t, repo)
	if len(names) != 2 {
		t.Fatalf("want two files, got %v", names)
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(repo, Dir, name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if strings.Contains(text, awsKey) || strings.Contains(text, ghToken) || strings.Contains(name, strings.ToLower(awsKey)) {
			t.Fatalf("a secret reached %s:\n%s", name, text)
		}
		f, err := Parse(text)
		if err != nil {
			t.Fatalf("%s does not parse: %v", name, err)
		}
		if hits := redact.Scan(f.Title + "\n" + f.Body + "\n" + strings.Join(f.Tags, "\n")); len(hits) > 0 {
			t.Fatalf("%s still matches redaction rules %v:\n%s", name, hits, text)
		}
	}
	// The catalogue's own copy is untouched: redaction is for what leaves.
	var title string
	m.db.QueryRow(`SELECT title FROM notes WHERE kind = 'gotcha'`).Scan(&title)
	if !strings.Contains(title, awsKey) {
		t.Fatal("export rewrote the stored note")
	}
}

// A file edited in the repository to contain a secret is redacted on the
// way in: nothing is stored unredacted.
func TestImportRedacts(t *testing.T) {
	m := newMachine(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, Dir), 0755); err != nil {
		t.Fatal(err)
	}
	const secret = "AKIAIOSFODNN7EXAMPLE"
	file := File{ID: "0123456789ab", Kind: "gotcha", Title: "Key rotation", Body: "The old key was " + secret + "."}
	if err := os.WriteFile(filepath.Join(repo, Dir, "gotcha-key-rotation-0123456789ab.md"), []byte(file.Render()), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(m.db, m.project, []string{repo}); err != nil {
		t.Fatal(err)
	}
	var body string
	m.db.QueryRow(`SELECT body FROM notes WHERE origin = 'repo'`).Scan(&body)
	if strings.Contains(body, secret) || !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("stored body: %q", body)
	}
}

func TestExportKeepsFilesCurrent(t *testing.T) {
	m := newMachine(t)
	repo := t.TempDir()
	id := m.note(t, "decision", "Use WAL mode", "Readers must not block the writer.", "")
	other := m.note(t, "fact", "CI runs on three systems", "", "")

	first, err := Export(m.db, m.project, repo, []int64{id}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing changed: nothing is rewritten, and the unnamed note stays out.
	again, err := Export(m.db, m.project, repo, nil, false)
	if err != nil || len(again.Written) != 0 || len(again.Unchanged) != 1 || len(files(t, repo)) != 1 {
		t.Fatalf("second export: %+v, %v, files %v", again, err, files(t, repo))
	}

	// The note is edited, title included: the same file is updated in place
	// rather than a second one appearing under the new title.
	if _, err := m.db.Exec(`UPDATE notes SET title = 'Always use WAL mode', body = 'Set it in the DSN.' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	updated, err := Export(m.db, m.project, repo, nil, false)
	if err != nil || len(updated.Written) != 1 || updated.Written[0] != first.Written[0] {
		t.Fatalf("export after an edit: %+v, %v (first was %v)", updated, err, first.Written)
	}
	content, _ := os.ReadFile(filepath.Join(repo, updated.Written[0]))
	if !strings.Contains(string(content), `title: "Always use WAL mode"`) || !strings.Contains(string(content), "Set it in the DSN.") {
		t.Fatalf("file not updated:\n%s", content)
	}

	// The note is superseded: its file is removed.
	if _, err := m.db.Exec(`UPDATE notes SET superseded_by = ? WHERE id = ?`, other, id); err != nil {
		t.Fatal(err)
	}
	removed, err := Export(m.db, m.project, repo, nil, false)
	if err != nil || len(removed.Removed) != 1 || len(files(t, repo)) != 0 {
		t.Fatalf("export after supersession: %+v, %v, files %v", removed, err, files(t, repo))
	}
}

func TestParse(t *testing.T) {
	good := File{ID: "0123456789ab", Kind: "decision", Title: `Quote "this": and a colon`, Body: "Line one.\n\n---\n\nA rule in the body is not a header.", Date: "2026-11-23", Agent: "codex", SourceCommit: "a1b2c3d4e5f6", Tags: []string{"a", "b c"}}
	got, err := Parse(good.Render())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != good.ID || got.Kind != good.Kind || got.Title != good.Title || got.Body != good.Body || got.Date != good.Date ||
		got.Agent != good.Agent || got.SourceCommit != good.SourceCommit || strings.Join(got.Tags, "|") != "a|b c" {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, good)
	}
	// Windows line endings, and a title someone typed without quotes.
	crlf := "---\r\nkae: 1\r\nid: 0123456789ab\r\nkind: fact\r\ntitle: Plain title\r\nunknown: ignored\r\n---\r\n\r\nBody.\r\n"
	if got, err := Parse(crlf); err != nil || got.Title != "Plain title" || got.Body != "Body." {
		t.Fatalf("crlf file: %+v, %v", got, err)
	}

	bad := map[string]string{
		"no header":         "Just some markdown.\n",
		"unclosed header":   "---\nid: 0123456789ab\nkind: fact\ntitle: x\n",
		"no id":             "---\nkind: fact\ntitle: x\n---\n\nBody\n",
		"an id of our own":  "---\nid: ../../etc\nkind: fact\ntitle: x\n---\n\nBody\n",
		"a kind not shared": "---\nid: 0123456789ab\nkind: todo\ntitle: x\n---\n\nBody\n",
		"no title":          "---\nid: 0123456789ab\nkind: fact\n---\n\nBody\n",
	}
	for name, content := range bad {
		if _, err := Parse(content); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// A file that is not a shared note is reported and skipped; the notes
// around it are still imported.
func TestImportSkipsWhatItCannotRead(t *testing.T) {
	m := newMachine(t)
	repo := t.TempDir()
	dir := filepath.Join(repo, Dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	good := File{ID: "aaaaaaaaaaaa", Kind: "fact", Title: "Deploys run from main"}
	os.WriteFile(filepath.Join(dir, "fact-deploys-aaaaaaaaaaaa.md"), []byte(good.Render()), 0644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Notes shared by kae\n"), 0644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not markdown, not ours"), 0644)

	got, err := Import(m.db, m.project, []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	if got.Added != 1 || len(got.Skipped) != 1 || !strings.Contains(got.Skipped[0], "README.md") {
		t.Fatalf("import: %+v", got)
	}
}

func TestImportAllUsesTheProjectsCheckout(t *testing.T) {
	m := newMachine(t)
	repo := t.TempDir()
	if _, err := m.db.Exec(`INSERT INTO project_paths(path, project_id) VALUES (?, ?)`, repo, m.project); err != nil {
		t.Fatal(err)
	}
	// A second project whose folder no longer exists: left alone.
	res, _ := m.db.Exec(`INSERT INTO projects(name) VALUES ('gone')`)
	gone, _ := res.LastInsertId()
	m.db.Exec(`INSERT INTO project_paths(path, project_id) VALUES (?, ?)`, filepath.Join(t.TempDir(), "deleted"), gone)
	m.db.Exec(`INSERT INTO notes(project_id, kind, title, origin, share_id) VALUES (?, 'fact', 'kept', 'repo', 'bbbbbbbbbbbb')`, gone)

	if err := os.MkdirAll(filepath.Join(repo, Dir), 0755); err != nil {
		t.Fatal(err)
	}
	f := File{ID: "cccccccccccc", Kind: "decision", Title: "Use pgx"}
	os.WriteFile(filepath.Join(repo, Dir, "decision-use-pgx-cccccccccccc.md"), []byte(f.Render()), 0644)

	got, err := ImportAll(m.db)
	if err != nil {
		t.Fatal(err)
	}
	if got.Added != 1 || got.Removed != 0 {
		t.Fatalf("ImportAll: %+v", got)
	}
	var kept int
	m.db.QueryRow(`SELECT count(*) FROM notes WHERE project_id = ? AND title = 'kept'`, gone).Scan(&kept)
	if kept != 1 {
		t.Fatal("a project with no checkout on this machine lost its shared notes")
	}
}
