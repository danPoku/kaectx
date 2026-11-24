// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package sharedmem moves notes between a machine's catalogue and files in
// the repository, so that what one person's agents learned reaches the
// others without a hosted service.
//
// The repository is already the thing a team shares, reviews and trusts. A
// note exported to .kae/memory/ is a small Markdown file: it shows up in a
// pull request like any other change, a reviewer can object to it, and
// `git pull` delivers it. Ingest on every machine with the checkout reads
// the folder back in. It is the office noticeboard next to each machine's
// private notebook: only what someone chose to pin up is on it, and anyone
// can see who pinned it and take it down.
//
// Only notes are shared here. Raw transcripts never go into the repository.
// Extracted decisions (#68) will use the same files when they exist.
package sharedmem

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/danPoku/kaectx/internal/redact"
	"github.com/danPoku/kaectx/internal/store"
)

// Dir is the folder, relative to the repository root, that holds shared
// notes.
var Dir = filepath.Join(".kae", "memory")

// formatVersion is written into every file as "kae: 1", so a later change to
// the file format can tell old files from new.
const formatVersion = "1"

// shareableKinds are the kinds of note worth circulating. A todo is one
// person's reminder, and a handoff is a snapshot of one session; neither is
// knowledge about the project.
var shareableKinds = map[string]bool{"decision": true, "gotcha": true, "convention": true, "fact": true}

// File is one shared note as it is written to disk.
type File struct {
	ID           string
	Kind         string
	Title        string
	Body         string
	Date         string // YYYY-MM-DD the note was first saved
	Agent        string
	SourceCommit string
	Tags         []string
}

// Render writes f as Markdown with a small header. String values are written
// as JSON strings, which are also valid YAML, so the header reads as YAML
// front matter to people and tools while being parsed here with nothing more
// than encoding/json.
func (f File) Render() string {
	var b strings.Builder
	quote := func(s string) string {
		data, _ := json.Marshal(s)
		return string(data)
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "kae: %s\n", formatVersion)
	fmt.Fprintf(&b, "id: %s\n", f.ID)
	fmt.Fprintf(&b, "kind: %s\n", f.Kind)
	fmt.Fprintf(&b, "title: %s\n", quote(f.Title))
	if f.Date != "" {
		fmt.Fprintf(&b, "date: %s\n", f.Date)
	}
	if f.Agent != "" {
		fmt.Fprintf(&b, "agent: %s\n", quote(f.Agent))
	}
	if f.SourceCommit != "" {
		fmt.Fprintf(&b, "source_commit: %s\n", f.SourceCommit)
	}
	if len(f.Tags) > 0 {
		tags, _ := json.Marshal(f.Tags)
		fmt.Fprintf(&b, "tags: %s\n", tags)
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(f.Body))
	b.WriteString("\n")
	return b.String()
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// Parse reads a shared note file. It is strict about the few things that
// must be right (the id, the kind, a title) and forgiving about the rest: a
// file is edited by hand and in merges, and one unknown header line should
// not make a note disappear.
func Parse(content string) (File, error) {
	var f File
	content = strings.ReplaceAll(content, "\r\n", "\n")
	rest, ok := strings.CutPrefix(content, "---\n")
	if !ok {
		return f, errors.New("does not start with a --- header")
	}
	header, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		// A file whose header is the last thing in it.
		header, ok = strings.CutSuffix(rest, "\n---")
		if !ok {
			return f, errors.New("header is not closed with ---")
		}
	}
	text := func(v string) string {
		v = strings.TrimSpace(v)
		var s string
		if strings.HasPrefix(v, `"`) && json.Unmarshal([]byte(v), &s) == nil {
			return s
		}
		return v
	}
	for _, line := range strings.Split(header, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "id":
			f.ID = text(value)
		case "kind":
			f.Kind = text(value)
		case "title":
			f.Title = text(value)
		case "date":
			f.Date = text(value)
		case "agent":
			f.Agent = text(value)
		case "source_commit":
			f.SourceCommit = text(value)
		case "tags":
			_ = json.Unmarshal([]byte(strings.TrimSpace(value)), &f.Tags)
		}
	}
	f.Body = strings.TrimSpace(body)
	switch {
	case !idPattern.MatchString(f.ID):
		return f, fmt.Errorf("id %q is not a kae share id", f.ID)
	case !shareableKinds[f.Kind]:
		return f, fmt.Errorf("kind %q cannot be shared (want decision, gotcha, convention or fact)", f.Kind)
	case strings.TrimSpace(f.Title) == "":
		return f, errors.New("title is empty")
	}
	return f, nil
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// fileName is the name a note is exported under: its kind, a readable slug
// of its title, and its id. The id keeps two notes with the same title
// apart, and keeps the name stable when the title is edited later.
func fileName(kind, title, id string) string {
	slug := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(slug) > 48 {
		slug = strings.Trim(slug[:48], "-")
	}
	if slug == "" {
		slug = "note"
	}
	return fmt.Sprintf("%s-%s-%s.md", kind, slug, id)
}

func newShareID() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Candidate is a local note that could be exported.
type Candidate struct {
	ID     int64
	Kind   string
	Title  string
	Shared bool // already exported once; export refreshes it
}

// Candidates lists the live local notes of a project that may be shared.
func Candidates(db *sql.DB, projectID int64) ([]Candidate, error) {
	rows, err := db.Query(`SELECT id, kind, title, share_id IS NOT NULL FROM notes
		WHERE project_id = ? AND origin = 'local' AND superseded_by IS NULL
		  AND kind IN ('decision', 'gotcha', 'convention', 'fact')
		ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.ID, &c.Kind, &c.Title, &c.Shared); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Exported says what Export did.
type Exported struct {
	// Written and Unchanged are file paths relative to the repository root.
	Written, Unchanged []string
	// Removed are files deleted because their note was superseded.
	Removed []string
}

// Export writes notes of projectID into root/.kae/memory.
//
// Naming a note is how it is approved for sharing: noteIDs are the notes to
// add, and all means every candidate. Notes exported before are refreshed on
// every run whether named or not, so an edit to a shared note reaches its
// file, and a shared note that has since been superseded has its file
// removed.
//
// Every file is passed through the redaction rules before it is written,
// title and body both. Notes are typed by agents and can quote a key as
// easily as a transcript can; a repository is the last place one should
// land.
func Export(db *sql.DB, projectID int64, root string, noteIDs []int64, all bool) (Exported, error) {
	var out Exported
	dir := filepath.Join(root, Dir)

	// Approve the named notes by giving them a share id.
	approve := map[int64]bool{}
	for _, id := range noteIDs {
		approve[id] = true
	}
	candidates, err := Candidates(db, projectID)
	if err != nil {
		return out, err
	}
	known := map[int64]bool{}
	for _, c := range candidates {
		known[c.ID] = true
		if c.Shared || !(all || approve[c.ID]) {
			continue
		}
		id, err := newShareID()
		if err != nil {
			return out, err
		}
		if _, err := db.Exec(`UPDATE notes SET share_id = ? WHERE id = ? AND share_id IS NULL`, id, c.ID); err != nil {
			return out, err
		}
	}
	for _, id := range noteIDs {
		if !known[id] {
			return out, fmt.Errorf("note %d cannot be shared: it is not a live decision, gotcha, convention or fact of this project", id)
		}
	}

	rows, err := db.Query(`SELECT n.share_id, n.kind, n.title, n.body, n.created_at, coalesce(n.agent, ''), n.tags,
			n.superseded_by IS NOT NULL, coalesce(s.ending_commit, '')
		FROM notes n LEFT JOIN sessions s ON s.id = n.session_id
		WHERE n.project_id = ? AND n.origin = 'local' AND n.share_id IS NOT NULL
		ORDER BY n.id`, projectID)
	if err != nil {
		return out, err
	}
	type shared struct {
		file       File
		superseded bool
	}
	var notes []shared
	for rows.Next() {
		var s shared
		var created, tags string
		if err := rows.Scan(&s.file.ID, &s.file.Kind, &s.file.Title, &s.file.Body, &created, &s.file.Agent, &tags, &s.superseded, &s.file.SourceCommit); err != nil {
			rows.Close()
			return out, err
		}
		if len(created) >= 10 {
			s.file.Date = created[:10]
		}
		if len(s.file.SourceCommit) > 12 {
			s.file.SourceCommit = s.file.SourceCommit[:12]
		}
		_ = json.Unmarshal([]byte(tags), &s.file.Tags)
		notes = append(notes, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	// Files already in the folder, by share id: a note keeps the file it has
	// even if its title (and so the name it would get today) has changed.
	existing, err := filesByID(dir)
	if err != nil {
		return out, err
	}
	for _, n := range notes {
		// Redact before anything is derived from the note. The file's name
		// is built from the title, so a key pasted into a title would
		// otherwise be published in the name even with the contents clean.
		n.file.Title = redact.Redact(n.file.Title)
		n.file.Body = redact.Redact(n.file.Body)
		for i, tag := range n.file.Tags {
			n.file.Tags[i] = redact.Redact(tag)
		}
		rel := filepath.Join(Dir, fileName(n.file.Kind, n.file.Title, n.file.ID))
		if have, ok := existing[n.file.ID]; ok {
			rel = filepath.Join(Dir, filepath.Base(have))
		}
		path := filepath.Join(root, rel)
		if n.superseded {
			if err := os.Remove(path); err == nil {
				out.Removed = append(out.Removed, rel)
			} else if !os.IsNotExist(err) {
				return out, err
			}
			continue
		}
		// Belt and braces: after redacting each field, the text that came
		// from the note is checked once more as a whole, and a note that
		// still matches a rule is not written at all. The header lines kae
		// itself generates (the id, the commit) are left out of the check:
		// they are random-looking by nature, and the rules would flag them.
		if hits := redact.Scan(n.file.Title + "\n" + n.file.Body + "\n" + strings.Join(n.file.Tags, "\n")); len(hits) > 0 {
			return out, fmt.Errorf("note %q still matches redaction rule(s) %v after redaction; not written", n.file.Title, hits)
		}
		content := n.file.Render()
		if old, err := os.ReadFile(path); err == nil && string(old) == content {
			out.Unchanged = append(out.Unchanged, rel)
			continue
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return out, err
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return out, err
		}
		out.Written = append(out.Written, rel)
	}
	return out, nil
}

// filesByID maps the share id of every readable note file in dir to its
// path. Files that do not parse are left out; Import reports those.
func filesByID(dir string) (map[string]string, error) {
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if f, err := Parse(string(data)); err == nil {
			out[f.ID] = path
		}
	}
	return out, nil
}

// Imported says what Import did for one project.
type Imported struct {
	Added, Updated, Removed int
	// Skipped lists files that could not be read as shared notes, with why.
	Skipped []string
}

func (i *Imported) add(o Imported) {
	i.Added += o.Added
	i.Updated += o.Updated
	i.Removed += o.Removed
	i.Skipped = append(i.Skipped, o.Skipped...)
}

// Import makes projectID's repo-origin notes match the files under each of
// roots: a new file becomes a note, an edited file updates its note, and a
// note whose file is gone is deleted. roots are the project's checkouts on
// this machine; when one id appears in several, the first root given wins.
//
// A file whose id belongs to a local note here is this machine's own export
// and is skipped, so the exporter does not see its note twice.
//
// Text is redacted on the way in as well. The file was redacted when it was
// exported, but it has been in a repository since, where anyone could have
// edited it; nothing reaches the database unredacted.
func Import(db *sql.DB, projectID int64, roots []string) (Imported, error) {
	var out Imported
	files := map[string]File{}
	for _, root := range roots {
		dir := filepath.Join(root, Dir)
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				out.Skipped = append(out.Skipped, fmt.Sprintf("%s: %v", path, err))
				continue
			}
			f, err := Parse(string(data))
			if err != nil {
				out.Skipped = append(out.Skipped, fmt.Sprintf("%s: %v", path, err))
				continue
			}
			if _, seen := files[f.ID]; !seen {
				files[f.ID] = f
			}
		}
	}

	tx, err := db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	ids := make([]string, 0, len(files))
	for id := range files {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		f := files[id]
		var own int
		if err := tx.QueryRow(`SELECT count(*) FROM notes WHERE project_id = ? AND share_id = ? AND origin = 'local'`, projectID, id).Scan(&own); err != nil {
			return out, err
		}
		if own > 0 {
			continue
		}
		title, body := redact.Redact(f.Title), redact.Redact(f.Body)
		tags := f.Tags
		if tags == nil {
			tags = []string{}
		}
		for i := range tags {
			tags[i] = redact.Redact(tags[i])
		}
		tagsJSON, err := json.Marshal(tags)
		if err != nil {
			return out, err
		}
		agent := f.Agent
		if agent == "" {
			agent = "repo"
		}

		var noteID int64
		var oldKind, oldTitle, oldBody, oldTags string
		err = tx.QueryRow(`SELECT id, kind, title, body, tags FROM notes WHERE project_id = ? AND share_id = ? AND origin = 'repo'`, projectID, id).
			Scan(&noteID, &oldKind, &oldTitle, &oldBody, &oldTags)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(`INSERT INTO notes(project_id, agent, kind, title, body, tags, origin, share_id)
				VALUES (?, ?, ?, ?, ?, ?, 'repo', ?)`, projectID, agent, f.Kind, title, body, string(tagsJSON), id); err != nil {
				return out, err
			}
			out.Added++
		case err != nil:
			return out, err
		case oldKind != f.Kind || oldTitle != title || oldBody != body || oldTags != string(tagsJSON):
			if _, err := tx.Exec(`UPDATE notes SET kind = ?, title = ?, body = ?, tags = ?, agent = ?,
				updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = ?`, f.Kind, title, body, string(tagsJSON), agent, noteID); err != nil {
				return out, err
			}
			out.Updated++
		}
	}

	// Notes whose file is no longer in any checkout.
	rows, err := tx.Query(`SELECT id, share_id FROM notes WHERE project_id = ? AND origin = 'repo'`, projectID)
	if err != nil {
		return out, err
	}
	var gone []int64
	for rows.Next() {
		var noteID int64
		var shareID string
		if err := rows.Scan(&noteID, &shareID); err != nil {
			rows.Close()
			return out, err
		}
		if _, ok := files[shareID]; !ok {
			gone = append(gone, noteID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for _, noteID := range gone {
		if _, err := tx.Exec(`DELETE FROM notes WHERE id = ?`, noteID); err != nil {
			return out, err
		}
		out.Removed++
	}
	return out, tx.Commit()
}

// ImportAll runs Import for every project that has a checkout on this
// machine. It is what ingest calls after reading the agents' logs.
//
// A project with no checkout here is left as it is: a folder that was
// deleted or is on an unmounted drive says nothing about whether its shared
// notes still exist.
func ImportAll(db *sql.DB) (Imported, error) {
	var total Imported
	rows, err := db.Query(`SELECT project_id, path FROM project_paths ORDER BY project_id, path`)
	if err != nil {
		return total, err
	}
	byProject := map[int64][]string{}
	var order []int64
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return total, err
		}
		if _, seen := byProject[id]; !seen {
			order = append(order, id)
		}
		byProject[id] = append(byProject[id], path)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return total, err
	}

	git := store.NewGitResolver()
	for _, id := range order {
		var roots []string
		seen := map[string]bool{}
		for _, path := range byProject[id] {
			if info, err := os.Stat(path); err != nil || !info.IsDir() {
				continue
			}
			root := git.Root(path)
			if root == "" {
				root = path
			}
			if !seen[root] {
				seen[root] = true
				roots = append(roots, root)
			}
		}
		if len(roots) == 0 {
			continue
		}
		got, err := Import(db, id, roots)
		if err != nil {
			return total, fmt.Errorf("project %d: %w", id, err)
		}
		total.add(got)
	}
	return total, nil
}
