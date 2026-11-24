// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/danPoku/kaectx/internal/brief"
	"github.com/danPoku/kaectx/internal/handoff"
	"github.com/danPoku/kaectx/internal/store"
)

// runHandoff handles `kae handoff`: write a handoff note for the project's
// latest session (or the one named by --session) and print it.
func runHandoff(args []string) error {
	fs := flag.NewFlagSet("handoff", flag.ContinueOnError)
	session := fs.String("session", "", "hand off this session (an id such as claude-code:91ae...) instead of the project's latest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: kae handoff [--session ID]")
	}
	db, projectID, err := openProject()
	if err != nil {
		return err
	}
	defer db.Close()
	return writeHandoff(os.Stdout, db, projectID, *session)
}

// runResume handles `kae resume`: print the project's newest handoff note,
// whichever agent or command wrote it.
func runResume(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: kae resume")
	}
	db, projectID, err := openProject()
	if err != nil {
		return err
	}
	defer db.Close()
	return printResume(os.Stdout, db, projectID)
}

// openProject opens the store and finds the project for the current
// directory. A folder kae has never seen is an error here, unlike in
// `kae brief`: these commands were asked for by name, so silence would be
// confusing.
func openProject() (*sql.DB, int64, error) {
	db, _, err := openStore()
	if err != nil {
		return nil, 0, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		db.Close()
		return nil, 0, fmt.Errorf("resolve working directory: %w", err)
	}
	projectID, found, err := store.LookupProject(db, cwd)
	if err != nil {
		db.Close()
		return nil, 0, err
	}
	if !found {
		db.Close()
		return nil, 0, fmt.Errorf("no kae project found for %s; ingest a session from this directory first", cwd)
	}
	return db, projectID, nil
}

func writeHandoff(w io.Writer, db *sql.DB, projectID int64, sessionID string) error {
	if sessionID == "" {
		latest, err := handoff.LatestSession(db, projectID)
		if err != nil {
			return err
		}
		sessionID = latest
	}
	note, err := handoff.Build(db, projectID, sessionID)
	if err != nil {
		return err
	}
	id, superseded, err := handoff.Save(db, projectID, note)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "saved handoff as note %d", id)
	if superseded != 0 {
		fmt.Fprintf(w, " (replaces note %d, the earlier handoff for this session)", superseded)
	}
	fmt.Fprintf(w, "\nPick it up in any agent with `kae resume` or the get_handoff tool.\n\n%s\n\n%s", note.Title, note.Body)
	return nil
}

// printResume prints the newest handoff and records that it was picked up.
// Unlike a brief read out of curiosity, `kae resume` is the pickup itself:
// someone is starting from where the last session stopped.
func printResume(w io.Writer, db *sql.DB, projectID int64) error {
	h, err := brief.LatestHandoff(db, projectID)
	if err != nil {
		return err
	}
	if h == nil {
		fmt.Fprintln(w, "no handoff saved for this project yet; `kae handoff` writes one from the latest session")
		return nil
	}
	if _, err := io.WriteString(w, handoff.Format(h)); err != nil {
		return err
	}
	return brief.RecordDelivery(db, h.NoteID)
}
