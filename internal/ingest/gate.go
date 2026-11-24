// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"database/sql"
	"errors"

	"github.com/danPoku/kaectx/internal/store"
)

// gate decides, for one ingest run of one file, whether a session's lines
// may be stored at all. Two things can close it: the session was forgotten
// (`kae forget`), or it belongs to a project the user excluded from ingest
// (`kae project exclude`).
//
// It is the doorman with two lists, checked before anything is written. The
// check sits here, in front of the session upsert, because the upsert is
// what would create the session's row and, for a new folder, its project;
// by then the thing the user asked not to keep is already kept.
//
// Decisions are cached for the run: a session's lines arrive by the hundred,
// and the answer for one is the answer for all of them.
type gate struct {
	db      *sql.DB
	blocked map[string]bool
	// skippedExcluded is set once a session was turned away because its
	// project is excluded (not because it was forgotten). The run records
	// it against the log file, so that including the project again can
	// send the file back to be read from the start.
	skippedExcluded bool
}

func newGate(db *sql.DB) *gate {
	return &gate{db: db, blocked: map[string]bool{}}
}

// closed reports whether lines of sessionID, logged in folder cwd, must not
// be stored.
func (g *gate) closed(sessionID, cwd string) (bool, error) {
	if blocked, known := g.blocked[sessionID]; known {
		return blocked, nil
	}
	forgotten, err := store.IsForgotten(g.db, sessionID)
	if err != nil {
		return false, err
	}
	if forgotten {
		g.blocked[sessionID] = true
		return true, nil
	}

	// A session kae already holds belongs to its project, wherever its
	// later lines say they were logged.
	var projectID sql.NullInt64
	err = g.db.QueryRow(`SELECT project_id FROM sessions WHERE id = ?`, sessionID).Scan(&projectID)
	switch {
	case err == nil && projectID.Valid:
		excluded, err := store.ProjectExcluded(g.db, projectID.Int64)
		if err != nil {
			return false, err
		}
		g.blocked[sessionID] = excluded
		g.skippedExcluded = g.skippedExcluded || excluded
		return excluded, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return false, err
	}

	// A session not yet stored (or stored without a project): ask which
	// project owns the folder, without creating one. Until a line names a
	// folder there is nothing to decide, and nothing is cached, so the
	// first line that does name one is still checked.
	if cwd == "" {
		return false, nil
	}
	id, found, err := store.LookupProject(g.db, cwd)
	if err != nil {
		return false, err
	}
	excluded := false
	if found {
		if excluded, err = store.ProjectExcluded(g.db, id); err != nil {
			return false, err
		}
	}
	g.blocked[sessionID] = excluded
	g.skippedExcluded = g.skippedExcluded || excluded
	return excluded, nil
}
