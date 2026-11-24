// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/danPoku/kaectx/internal/sharedmem"
	"github.com/danPoku/kaectx/internal/store"
)

// runExportMemory handles `kae export-memory`: write chosen notes into the
// repository as files, so they travel with the code.
//
// Nothing is shared by default. With no arguments it only lists what could
// be; a note is shared by naming it (or with --all), and from then on every
// run keeps its file up to date. The command writes files and stops there:
// committing them, and so deciding that the team should have them, is left
// to the person and to review.
func runExportMemory(args []string) error {
	fs := flag.NewFlagSet("export-memory", flag.ContinueOnError)
	all := fs.Bool("all", false, "share every decision, gotcha, convention and fact of this project")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"all": false})); err != nil {
		return err
	}
	var ids []int64
	for _, a := range fs.Args() {
		id, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			return fmt.Errorf("usage: kae export-memory [--all] [note-id ...]   (%q is not a note id)", a)
		}
		ids = append(ids, id)
	}
	db, projectID, err := openProject()
	if err != nil {
		return err
	}
	defer db.Close()
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	// Files go at the repository root, wherever inside it the command is
	// run, so there is one .kae/memory per repository and not one per folder.
	root := store.NewGitResolver().Root(cwd)
	if root == "" {
		root = cwd
	}
	return exportMemory(os.Stdout, db, projectID, root, ids, *all)
}

func exportMemory(w io.Writer, db *sql.DB, projectID int64, root string, ids []int64, all bool) error {
	candidates, err := sharedmem.Candidates(db, projectID)
	if err != nil {
		return err
	}
	shared := 0
	for _, c := range candidates {
		if c.Shared {
			shared++
		}
	}
	if len(ids) == 0 && !all && shared == 0 {
		if len(candidates) == 0 {
			fmt.Fprintln(w, "nothing to share: this project has no decision, gotcha, convention or fact notes")
			return nil
		}
		fmt.Fprintln(w, "Notes that can be shared through the repository (none are yet):")
		for _, c := range candidates {
			fmt.Fprintf(w, "  %4d  %-10s  %s\n", c.ID, c.Kind, c.Title)
		}
		fmt.Fprintln(w, "\nShare some with `kae export-memory <id> ...`, or all of them with `kae export-memory --all`.")
		return nil
	}

	got, err := sharedmem.Export(db, projectID, root, ids, all)
	if err != nil {
		return err
	}
	for _, f := range got.Written {
		fmt.Fprintln(w, "wrote    ", f)
	}
	for _, f := range got.Removed {
		fmt.Fprintln(w, "removed  ", f, "(its note was superseded)")
	}
	fmt.Fprintf(w, "%d written, %d unchanged, %d removed in %s\n", len(got.Written), len(got.Unchanged), len(got.Removed), root)
	if len(got.Written)+len(got.Removed) > 0 {
		fmt.Fprintln(w, "Review and commit them; teammates' agents see them after their next `kae ingest`.")
	}
	return nil
}

// importSharedNotes reads every project's .kae/memory folder after an
// ingest, and says what changed. A failure is a warning: the session logs
// were ingested, and one unreadable note file must not make that look like
// it failed.
func importSharedNotes(w io.Writer, db *sql.DB) {
	got, err := sharedmem.ImportAll(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kae: warning: reading shared notes from repositories: %v\n", err)
		return
	}
	for _, s := range got.Skipped {
		fmt.Fprintf(os.Stderr, "kae: warning: skipped shared note %s\n", s)
	}
	if got.Added+got.Updated+got.Removed > 0 {
		fmt.Fprintf(w, "shared notes from repositories: %d added, %d updated, %d removed\n", got.Added, got.Updated, got.Removed)
	}
}
