// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danPoku/kaectx/internal/brief"
	"github.com/danPoku/kaectx/internal/sharedmem"
)

func TestExportMemoryListsThenShares(t *testing.T) {
	db, projectID := handoffDB(t)
	repo := t.TempDir()

	// Nothing to share yet.
	var out bytes.Buffer
	if err := exportMemory(&out, db, projectID, repo, nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to share") {
		t.Fatalf("no notes:\n%s", out.String())
	}

	for _, n := range [][2]string{{"decision", "Use WAL mode"}, {"gotcha", "FTS5 needs a build tag"}, {"todo", "Ring the vendor"}} {
		if _, err := db.Exec(`INSERT INTO notes(project_id, kind, title, body) VALUES (?, ?, ?, 'Because.')`, projectID, n[0], n[1]); err != nil {
			t.Fatal(err)
		}
	}

	// With no arguments it lists the candidates and writes nothing: sharing
	// is a decision, not a default.
	out.Reset()
	if err := exportMemory(&out, db, projectID, repo, nil, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"   1  decision    Use WAL mode", "   2  gotcha      FTS5 needs a build tag", "kae export-memory <id>"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("listing lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Ring the vendor") {
		t.Errorf("a todo is offered for sharing:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(repo, sharedmem.Dir)); err == nil {
		t.Fatal("listing wrote files")
	}

	// Naming a note shares it.
	out.Reset()
	if err := exportMemory(&out, db, projectID, repo, []int64{1}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 written, 0 unchanged, 0 removed") || !strings.Contains(out.String(), "Review and commit them") {
		t.Fatalf("export output:\n%s", out.String())
	}
	entries, _ := os.ReadDir(filepath.Join(repo, sharedmem.Dir))
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "decision-use-wal-mode-") {
		t.Fatalf("files: %v", entries)
	}

	// From then on a bare run keeps it current, and says nothing changed.
	out.Reset()
	if err := exportMemory(&out, db, projectID, repo, nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "0 written, 1 unchanged, 0 removed") || strings.Contains(out.String(), "Review and commit") {
		t.Fatalf("second export output:\n%s", out.String())
	}

	// A note that cannot be shared is refused by name.
	if err := exportMemory(&out, db, projectID, repo, []int64{3}, false); err == nil || !strings.Contains(err.Error(), "note 3 cannot be shared") {
		t.Fatalf("exporting the todo: %v", err)
	}
}

// Notes a teammate shared are in the session-start brief, marked as coming
// from the repository.
func TestBriefShowsNotesSharedThroughTheRepo(t *testing.T) {
	db, projectID := handoffDB(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, sharedmem.Dir), 0755); err != nil {
		t.Fatal(err)
	}
	f := sharedmem.File{ID: "0123456789ab", Kind: "convention", Title: "Migrations are never edited after release"}
	if err := os.WriteFile(filepath.Join(repo, sharedmem.Dir, "convention-migrations-0123456789ab.md"), []byte(f.Render()), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := sharedmem.Import(db, projectID, []string{repo}); err != nil {
		t.Fatal(err)
	}
	b, err := brief.Build(db, projectID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	text := b.Render(brief.DefaultBudget).Text
	want := "Shared through the repo (.kae/memory):\n- convention: Migrations are never edited after release  search_notes(query=\"Migrations are never edited after release\")"
	if !strings.Contains(text, want) {
		t.Fatalf("brief lacks the shared note:\n%s", text)
	}
}
