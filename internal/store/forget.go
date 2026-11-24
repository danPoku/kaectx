// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Forgotten is what ForgetSession removed.
type Forgotten struct {
	SessionID                 string
	Messages, Chunks, Touches int
}

// ErrSessionNotFound means the session id names nothing in the catalogue.
var ErrSessionNotFound = errors.New("session not found")

// ForgetSession removes a session and everything stored from it, and records
// its id so ingest never brings it back.
//
// One transaction covers both halves. If the rows were deleted but the id
// were not recorded, the next ingest would re-read the log file and restore
// what the user asked to remove; recording first and failing to delete would
// leave the text in place. Like striking a name from a register and adding
// it to the list at the door in one act.
//
// Deleting the session row removes its messages, chunks (and through their
// triggers the search index entries and vectors) and file touches by ON
// DELETE CASCADE. Notes saved in the session are kept, with their link to it
// cleared: a note is something a person or agent chose to write down, not
// transcript. Search telemetry about the session keeps its ranges and
// scores, which hold none of the session's text.
func ForgetSession(db *sql.DB, sessionID string) (Forgotten, error) {
	out := Forgotten{SessionID: sessionID}
	tx, err := db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	var agent string
	err = tx.QueryRow(`SELECT agent FROM sessions WHERE id = ?`, sessionID).Scan(&agent)
	if errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	if err != nil {
		return out, err
	}
	for _, c := range []struct {
		query string
		into  *int
	}{
		{`SELECT count(*) FROM messages WHERE session_id = ?`, &out.Messages},
		{`SELECT count(*) FROM chunks WHERE session_id = ?`, &out.Chunks},
		{`SELECT count(*) FROM file_touches WHERE session_id = ?`, &out.Touches},
	} {
		if err := tx.QueryRow(c.query, sessionID).Scan(c.into); err != nil {
			return out, err
		}
	}

	if _, err := tx.Exec(`INSERT INTO forgotten_sessions(session_id, agent) VALUES (?, ?)
		ON CONFLICT(session_id) DO NOTHING`, sessionID, agent); err != nil {
		return out, err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE id = ?`, sessionID); err != nil {
		return out, err
	}
	// The unknown-line tally is kept per session and is not tied to the
	// session row, so it is cleared by hand.
	if _, err := tx.Exec(`DELETE FROM unknown_line_types WHERE session_id = ?`, sessionID); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// IsForgotten reports whether sessionID is on the do-not-readmit list.
func IsForgotten(db DBTX, sessionID string) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM forgotten_sessions WHERE session_id = ?`, sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// SetProjectExcluded turns ingest off (excluded true) or back on for a
// project. It reports whether the project's state changed, so a caller can
// say "already excluded" instead of claiming to have done something.
func SetProjectExcluded(db *sql.DB, projectID int64, excluded bool) (changed bool, err error) {
	query := `UPDATE projects SET excluded_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = ? AND excluded_at IS NULL`
	if !excluded {
		query = `UPDATE projects SET excluded_at = NULL WHERE id = ? AND excluded_at IS NOT NULL`
	}
	res, err := db.Exec(query, projectID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		if !excluded {
			if err := rereadSkippedSources(db); err != nil {
				return true, err
			}
		}
		return true, nil
	}
	var one int
	if err := db.QueryRow(`SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("project %d not found (see `kae project list`)", projectID)
	} else if err != nil {
		return false, err
	}
	return false, nil
}

// RereadMarker is written over a source's head hash to make the next ingest
// read the file again from its first byte. Ingest compares the stored hash
// with the file's; a value no hash can equal is the standing instruction
// "your bookmark is void", and ingest then replaces the file's sessions
// rather than appending to them, exactly as it does for a rewritten file.
const RereadMarker = "reread-after-include"

// rereadSkippedSources sends every log file that had lines passed over
// because of an exclusion back to be read from the start. It cannot tell
// which excluded project a file's skipped lines belonged to (nothing was
// stored about them, by design), so it flags all of them; a file whose
// project is still excluded is simply skipped again and flagged again.
func rereadSkippedSources(db *sql.DB) error {
	_, err := db.Exec(`UPDATE sources SET content_head_hash = ?, skipped_excluded = 0 WHERE skipped_excluded = 1`, RereadMarker)
	return err
}

// ProjectExcluded reports whether ingest is turned off for a project.
func ProjectExcluded(db DBTX, projectID int64) (bool, error) {
	var at sql.NullString
	err := db.QueryRow(`SELECT excluded_at FROM projects WHERE id = ?`, projectID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return at.Valid, err
}

// ForgetProjectSessions forgets every session of a project, for "exclude
// this repository and erase what was collected". It returns how many
// sessions were removed.
func ForgetProjectSessions(db *sql.DB, projectID int64) (int, error) {
	rows, err := db.Query(`SELECT id FROM sessions WHERE project_id = ?`, projectID)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for i, id := range ids {
		if _, err := ForgetSession(db, id); err != nil {
			return i, fmt.Errorf("forget %s: %w", id, err)
		}
	}
	return len(ids), nil
}

// FindSession resolves what a person typed to a session id: the full id
// ('claude-code:91ae043d-...'), the agent's own id without the agent prefix,
// or an unambiguous prefix of either. It never guesses between two sessions.
func FindSession(db *sql.DB, typed string) (string, error) {
	typed = strings.TrimSpace(typed)
	if typed == "" {
		return "", errors.New("no session given")
	}
	// Tried from most to least exact, stopping at the first form that
	// matches anything. Two agents can use the same native id (both call
	// their first session "sess-1" in tests, and UUIDs make it unlikely but
	// not impossible in life), so even an exact native id can be ambiguous.
	for _, form := range []struct {
		where string
		arg   string
	}{
		{`id = ?`, typed},
		{`native_id = ?`, typed},
		{`id LIKE ? ESCAPE '\' OR native_id LIKE ? ESCAPE '\'`, likePrefix(typed)},
	} {
		args := []any{form.arg}
		if strings.Count(form.where, "?") == 2 {
			args = append(args, form.arg)
		}
		ids, err := sessionIDs(db, `SELECT id FROM sessions WHERE `+form.where+` ORDER BY id LIMIT 6`, args...)
		if err != nil {
			return "", err
		}
		switch len(ids) {
		case 0:
			continue
		case 1:
			return ids[0], nil
		}
		return "", fmt.Errorf("%q matches more than one session (%s); give more of the id", typed, strings.Join(ids, ", "))
	}
	return "", fmt.Errorf("%w: %s", ErrSessionNotFound, typed)
}

func sessionIDs(db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// likePrefix turns s into a LIKE pattern matching strings that start with
// it, with LIKE's own wildcards in s escaped.
func likePrefix(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
}
