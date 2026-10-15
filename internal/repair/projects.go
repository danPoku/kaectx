// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package repair

import (
	"database/sql"
	"fmt"

	"github.com/danPoku/kaectx/internal/store"
)

// ProjectsResult counts what a Projects run found and changed. With DryRun
// the counts describe what would have changed; nothing is written.
type ProjectsResult struct {
	Examined  int    // sessions with a cwd to judge by: a Claude Code main-transcript line, or another agent's logged cwd
	NoMain    int    // Claude Code sessions whose stored lines all came from subagents; left as they were
	NoCWD     int    // other agents' sessions with no logged cwd; left as they were
	Unknown   int    // sessions whose first cwd no known project owns; left as they were
	Restarted int    // sessions whose started_at moved earlier
	Moved     []Move // sessions whose project changed, in session id order
}

// Move is one session filed under a different project.
type Move struct {
	SessionID string
	From, To  string // project names; From is "" when the session had none
}

// Projects re-files Claude Code sessions under the project their main
// transcript starts in, for databases ingested before subagent transcripts
// were told apart. A subagent transcript reuses its parent's session id, and
// it was often read first (WalkDir visits "<id>/" before "<id>.jsonl"), so
// its cwd chose the session's project, and its first timestamp became
// started_at.
//
// The original lines are in messages.raw, so no log file is needed: the
// earliest line without isSidechain decides the project, and the earliest
// message of any kind decides started_at. Like the other repairs it runs in
// one transaction and is idempotent: a second run moves nothing.
//
// Every other agent's session is re-filed by its logged cwd. That catches
// sessions filed before project lookup ignored path spelling and mapped git
// worktrees to their repo: a Codex session in C:\x\.worktrees\api-1 used to
// land in the C:\x project, and one folder spelled C:\x and /mnt/c/x could be
// two projects.
func Projects(db *sql.DB, dryRun bool) (ProjectsResult, error) {
	var res ProjectsResult
	tx, err := db.Begin()
	if err != nil {
		return res, fmt.Errorf("begin repair: %w", err)
	}
	defer tx.Rollback()

	type session struct {
		id, agent, cwd string
		project        sql.NullInt64
	}
	rows, err := tx.Query(`SELECT id, agent, COALESCE(cwd, ''), project_id FROM sessions ORDER BY id`)
	if err != nil {
		return res, fmt.Errorf("list sessions: %w", err)
	}
	var sessions []session
	for rows.Next() {
		var s session
		if err := rows.Scan(&s.id, &s.agent, &s.cwd, &s.project); err != nil {
			rows.Close()
			return res, err
		}
		sessions = append(sessions, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	for _, s := range sessions {
		cwd := s.cwd
		if s.agent == "claude-code" {
			// Messages with no timestamp sort last, not first: ORDER BY puts
			// NULL ahead of every value.
			err := tx.QueryRow(`SELECT json_extract(raw, '$.cwd') FROM messages
			                    WHERE session_id = ? AND raw IS NOT NULL
			                      AND COALESCE(json_extract(raw, '$.isSidechain'), 0) = 0
			                      AND COALESCE(json_extract(raw, '$.cwd'), '') <> ''
			                    ORDER BY created_at IS NULL, created_at, seq LIMIT 1`, s.id).Scan(&cwd)
			if err == sql.ErrNoRows {
				res.NoMain++
				continue
			}
			if err != nil {
				return res, fmt.Errorf("first cwd of %s: %w", s.id, err)
			}

			// An empty started_at comes from a row created by a line with no
			// timestamp (an ai-title read first); any real one beats it.
			r, err := tx.Exec(`WITH first AS (SELECT MIN(NULLIF(created_at, '')) AS at FROM messages WHERE session_id = ?1)
			                   UPDATE sessions SET started_at = (SELECT at FROM first)
			                   WHERE id = ?1 AND (SELECT at FROM first) IS NOT NULL
			                     AND (started_at = '' OR (SELECT at FROM first) < started_at)`, s.id)
			if err != nil {
				return res, fmt.Errorf("started_at of %s: %w", s.id, err)
			}
			if n, _ := r.RowsAffected(); n > 0 {
				res.Restarted++
			}
		} else if cwd == "" {
			res.NoCWD++
			continue
		}
		res.Examined++

		// Look up, never create: a cwd no known project owns (say a Linux
		// /home path read on Windows) says nothing about where the session
		// belongs, and a repair that minted projects would split the
		// catalogue it is meant to fix.
		id, ok, err := store.LookupProject(tx, cwd)
		if err != nil {
			return res, fmt.Errorf("look up project of %s: %w", s.id, err)
		}
		if !ok {
			res.Unknown++
			continue
		}
		if s.project.Valid && s.project.Int64 == id {
			// Right already, but a provisional marker left in place would
			// let the next ingested line move it again.
			if _, err := tx.Exec(`UPDATE sessions SET meta = json_remove(meta, '$.provisional_project')
			                      WHERE id = ? AND json_extract(meta, '$.provisional_project') IS NOT NULL`, s.id); err != nil {
				return res, fmt.Errorf("settle %s: %w", s.id, err)
			}
			continue
		}
		if _, err := tx.Exec(`UPDATE sessions SET project_id = ?, meta = json_remove(meta, '$.provisional_project')
		                      WHERE id = ?`, id, s.id); err != nil {
			return res, fmt.Errorf("move %s: %w", s.id, err)
		}
		if _, err := tx.Exec(`UPDATE file_touches SET project_id = ? WHERE session_id = ?`, id, s.id); err != nil {
			return res, fmt.Errorf("move file records of %s: %w", s.id, err)
		}
		m := Move{SessionID: s.id}
		if s.project.Valid {
			if m.From, err = projectName(tx, s.project.Int64); err != nil {
				return res, err
			}
		}
		if m.To, err = projectName(tx, id); err != nil {
			return res, err
		}
		res.Moved = append(res.Moved, m)
	}

	if dryRun {
		return res, nil // deferred Rollback discards the writes
	}
	return res, tx.Commit()
}

func projectName(tx *sql.Tx, id int64) (string, error) {
	var name string
	if err := tx.QueryRow(`SELECT name FROM projects WHERE id = ?`, id).Scan(&name); err != nil {
		return "", fmt.Errorf("project %d: %w", id, err)
	}
	return fmt.Sprintf("%s (%d)", name, id), nil
}
