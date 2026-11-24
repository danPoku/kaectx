// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"database/sql"
	"time"
)

// unknownKey identifies one kind of unrecognised line within a file: the
// session it belongs to, its type, and the agent version that wrote it.
type unknownKey struct {
	sessionID string // '<agent>:<native_id>', or "" when the line named none
	lineType  string
	version   string
}

// unknownSeen is how often a kind was met in one run, and between when.
type unknownSeen struct {
	count       int
	first, last string
}

// unknownTally collects the unrecognised lines of one ingest run of one
// file. It is written to the database once, at the end of the run, rather
// than line by line: a log with thousands of lines of a new type would
// otherwise cost thousands of writes to say one thing.
type unknownTally map[unknownKey]*unknownSeen

// add records one line of type lineType, belonging to sessionID and written
// by agent version version at time at (the log line's own timestamp, or ""
// when it has none).
func (t unknownTally) add(sessionID, lineType, version, at string) {
	if at == "" {
		at = time.Now().UTC().Format(time.RFC3339)
	}
	k := unknownKey{sessionID: sessionID, lineType: lineType, version: version}
	seen := t[k]
	if seen == nil {
		t[k] = &unknownSeen{count: 1, first: at, last: at}
		return
	}
	seen.count++
	if at < seen.first {
		seen.first = at
	}
	if at > seen.last {
		seen.last = at
	}
}

// recordUnknown stores a run's tally against its source file.
//
// Counts must survive a re-run without doubling. Two things make that true.
// A normal run only reads bytes it has not read before, so its tally holds
// only new lines and is added to what is stored. A rewound run (the file was
// rewritten, so it is read again from the first byte) would count every line
// a second time, so the file's stored rows are cleared first and the tally
// replaces them, exactly as the file's messages are replaced.
func recordUnknown(db *sql.DB, sourceID int64, agent string, rewound bool, tally unknownTally) error {
	if !rewound && len(tally) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if rewound {
		if _, err := tx.Exec(`DELETE FROM unknown_line_types WHERE source_id = ?`, sourceID); err != nil {
			return err
		}
	}
	for k, seen := range tally {
		if _, err := tx.Exec(`INSERT INTO unknown_line_types
				(source_id, agent, session_id, line_type, agent_version, line_count, first_seen_at, last_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(source_id, session_id, line_type, agent_version) DO UPDATE SET
				line_count    = line_count + excluded.line_count,
				first_seen_at = min(first_seen_at, excluded.first_seen_at),
				last_seen_at  = max(last_seen_at, excluded.last_seen_at)`,
			sourceID, agent, k.sessionID, k.lineType, k.version, seen.count, seen.first, seen.last); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UnknownLines is one kind of unrecognised log line, summed over the files
// it appeared in.
type UnknownLines struct {
	Agent    string
	LineType string
	// Version is the agent version that wrote the lines, "" if unknown.
	Version string
	// Lines is how many such lines were skipped, in Files log files.
	Lines, Files int
	// FirstSeen and LastSeen are ISO-8601 UTC times.
	FirstSeen, LastSeen string
}

// ListUnknown returns the unrecognised line types on record, most recently
// seen first.
//
// projectID limits the answer to lines that belong to a session of that
// project; 0 means every line, including those that named no session.
// since, when not empty, keeps only types last seen at or after that time,
// so a format quirk that an adapter update has since dealt with stops being
// reported once its lines age out.
func ListUnknown(db *sql.DB, projectID int64, since string, limit int) ([]UnknownLines, error) {
	rows, err := db.Query(`SELECT agent, line_type, agent_version, sum(line_count), count(DISTINCT source_id),
			min(first_seen_at), max(last_seen_at)
		FROM unknown_line_types u
		WHERE (?1 = 0 OR EXISTS (SELECT 1 FROM sessions s WHERE s.id = u.session_id AND s.project_id = ?1))
		GROUP BY agent, line_type, agent_version
		HAVING max(last_seen_at) >= ?2
		ORDER BY max(last_seen_at) DESC, agent, line_type, agent_version
		LIMIT ?3`, projectID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnknownLines
	for rows.Next() {
		var u UnknownLines
		if err := rows.Scan(&u.Agent, &u.LineType, &u.Version, &u.Lines, &u.Files, &u.FirstSeen, &u.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
