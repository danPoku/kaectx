// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// MergeProjects folds project `from` into project `into` and deletes `from`.
//
// Why this exists: projects are identified by normalised git remote, but a
// session whose cwd has since vanished (a deleted or moved checkout) can no
// longer have its remote read, so it lands in a remote-less project of its
// own and project-scoped search never finds it. Think of two client files
// for the same client, opened before anyone knew they were the same person:
// merging moves every page into one folder and shreds the empty one.
//
// Rules:
//   - from and into must differ and both must exist.
//   - If both carry a git_remote they are different repos by definition, so
//     the merge is refused. If only `from` has one, `into` inherits it.
//   - Vectors of moved chunks are dropped and those chunks are marked
//     pending again: chunk_vectors.project_id is vec0's PARTITION KEY and
//     can't be updated in place (see internal/embed/worker.go), so the
//     embed worker simply re-embeds them under the new project.
//
// Everything happens in one transaction; a failure leaves both projects
// untouched.
func MergeProjects(db *sql.DB, from, into int64) error {
	if from == into {
		return errors.New("cannot merge a project into itself")
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	fromRemote, err := projectRemote(tx, from)
	if err != nil {
		return err
	}
	intoRemote, err := projectRemote(tx, into)
	if err != nil {
		return err
	}
	if fromRemote.Valid && intoRemote.Valid {
		return fmt.Errorf("both projects have a git remote (%s and %s): they are different repos, refusing to merge", fromRemote.String, intoRemote.String)
	}

	// Drop vectors first, while the chunks still point at `from` via their
	// sessions. Skipped entirely when the optional vec0 table doesn't exist.
	var hasVec int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'chunk_vectors'`).Scan(&hasVec); err != nil {
		return err
	}
	if hasVec > 0 {
		if _, err := tx.Exec(`DELETE FROM chunk_vectors WHERE chunk_id IN (
			SELECT c.id FROM chunks c JOIN sessions s ON s.id = c.session_id WHERE s.project_id = ?)`, from); err != nil {
			return fmt.Errorf("drop vectors: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE chunks SET embedded_at = NULL, embedding_model = NULL
		WHERE session_id IN (SELECT id FROM sessions WHERE project_id = ?)`, from); err != nil {
		return fmt.Errorf("requeue chunks: %w", err)
	}

	for _, table := range []string{"sessions", "file_touches", "notes", "project_paths"} {
		if _, err := tx.Exec(`UPDATE `+table+` SET project_id = ? WHERE project_id = ?`, into, from); err != nil {
			return fmt.Errorf("move %s: %w", table, err)
		}
	}

	// Hits that other projects' all-projects searches found in `from` now
	// live in `into`. Without this the delete below would null the link, and
	// a NULL reads as "the searching project's own hit".
	if _, err := tx.Exec(`UPDATE retrieval_candidates SET project_id = ? WHERE project_id = ?`, into, from); err != nil {
		return fmt.Errorf("move retrieval candidates: %w", err)
	}

	// git_remote is UNIQUE, so free it on `from` (by deleting the row)
	// before `into` can take it over.
	if _, err := tx.Exec(`DELETE FROM projects WHERE id = ?`, from); err != nil {
		return err
	}
	if fromRemote.Valid {
		if _, err := tx.Exec(`UPDATE projects SET git_remote = ? WHERE id = ?`, fromRemote.String, into); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// projectRemote returns the project's git_remote (NULL-able) and errors if
// the project doesn't exist.
func projectRemote(tx *sql.Tx, id int64) (sql.NullString, error) {
	var remote sql.NullString
	err := tx.QueryRow(`SELECT git_remote FROM projects WHERE id = ?`, id).Scan(&remote)
	if errors.Is(err, sql.ErrNoRows) {
		return remote, fmt.Errorf("no project with id %d", id)
	}
	return remote, err
}
