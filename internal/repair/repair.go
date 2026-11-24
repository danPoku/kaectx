// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package repair re-derives data that depends on git, for databases written
// before kae computed it correctly. It works from what is already in the
// database (session cwd, branch, message timestamps, stored paths), so it
// needs no access to the original log files.
package repair

import (
	"database/sql"
	"fmt"
	"os"

	"github.com/danPoku/kaectx/internal/ingest/codex"
	"github.com/danPoku/kaectx/internal/store"
)

// Result counts what a Git run found and changed. With DryRun the counts
// describe what would have changed; nothing is written.
type Result struct {
	Sessions int // sessions with a working directory that were examined

	CommitsUpdated    int // sessions whose starting/ending commit or commit_source changed
	CommitsUnresolved int // sessions where neither commit could be resolved; commits left as they were
	CommitsUnverified int // of those, sessions newly marked "unverified" because they still carry a commit we can't re-derive

	TouchesRewritten int // file_touches whose path changed to be repo-root-relative
	TouchesRooted    int // file_touches now marked as repo-root-anchored (includes the rewritten ones)
	TouchesPending   int // file_touches left alone because their directory no longer exists; retried next run
}

// Git does two things, both idempotent:
//
//  1. Recomputes every session's starting_commit/ending_commit from the
//     timestamps of its first and last messages, replacing values written by
//     earlier builds that stamped the HEAD at ingest time. A session whose
//     repository can no longer be read keeps whatever it had: an unverifiable
//     value is better left alone than blanked.
//  2. Rewrites file_touches.path from cwd-relative to repo-root-relative for
//     rows not yet marked rooted, then marks them. The marker is what makes a
//     second run safe: without it the subdirectory prefix would be added again.
//
// It runs in one transaction, so an interrupted repair changes nothing.
func Git(db *sql.DB, git *store.GitResolver, dryRun bool) (Result, error) {
	var res Result
	tx, err := db.Begin()
	if err != nil {
		return res, fmt.Errorf("begin repair: %w", err)
	}
	defer tx.Rollback()

	if err := repairCommits(tx, git, &res); err != nil {
		return res, err
	}
	if err := repairPaths(tx, git, &res); err != nil {
		return res, err
	}
	if dryRun {
		return res, nil // deferred Rollback discards the writes
	}
	return res, tx.Commit()
}

type sessionRow struct {
	id, cwd, branch, startTS, endTS string
	start, end, source              string
}

func repairCommits(tx *sql.Tx, git *store.GitResolver, res *Result) error {
	// A session's span is its first to last message; sessions with no
	// timestamped messages fall back to the session's own started/ended times.
	rows, err := tx.Query(`
		SELECT s.id, s.cwd, COALESCE(s.git_branch, ''),
		       COALESCE(MIN(m.created_at), s.started_at),
		       COALESCE(MAX(m.created_at), s.ended_at, s.started_at),
		       COALESCE(s.starting_commit, ''), COALESCE(s.ending_commit, ''),
		       COALESCE(s.commit_source, '')
		  FROM sessions s
		  LEFT JOIN messages m ON m.session_id = s.id
		 WHERE s.cwd IS NOT NULL AND s.cwd <> ''
		 GROUP BY s.id`)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	var sessions []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.id, &r.cwd, &r.branch, &r.startTS, &r.endTS, &r.start, &r.end, &r.source); err != nil {
			rows.Close()
			return err
		}
		sessions = append(sessions, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, r := range sessions {
		res.Sessions++
		start, startSrc := git.Resolve(r.cwd, r.branch, r.startTS)
		end, endSrc := git.Resolve(r.cwd, r.branch, r.endTS)
		if start == "" && end == "" {
			res.CommitsUnresolved++
			// A commit we can no longer re-derive may be one an earlier
			// build stamped from HEAD at ingest time. Keep it, but say so.
			if (r.start != "" || r.end != "") && r.source != store.SourceUnverified {
				if _, err := tx.Exec(`UPDATE sessions SET commit_source = ? WHERE id = ?`, store.SourceUnverified, r.id); err != nil {
					return fmt.Errorf("mark session %s unverified: %w", r.id, err)
				}
				res.CommitsUnverified++
			}
			continue
		}
		// One end may resolve where the other predates the repository's
		// history; keep the old value for the end we couldn't derive, and
		// let its source stand only if it was already worse.
		src := worse(startSrc, endSrc)
		if start == "" && r.start != "" {
			start = r.start
			src = worse(src, store.SourceUnverified)
		}
		if end == "" && r.end != "" {
			end = r.end
			src = worse(src, store.SourceUnverified)
		}
		if start == r.start && end == r.end && src == r.source {
			continue
		}
		if _, err := tx.Exec(`UPDATE sessions SET starting_commit = ?, ending_commit = ?, commit_source = ? WHERE id = ?`,
			nullIfEmpty(start), nullIfEmpty(end), nullIfEmpty(src), r.id); err != nil {
			return fmt.Errorf("update session %s: %w", r.id, err)
		}
		res.CommitsUpdated++
	}
	return nil
}

// worse returns the less trustworthy of two commit sources.
func worse(a, b string) string {
	if store.SourceRank[b] > store.SourceRank[a] {
		return b
	}
	return a
}

// anchorable reports whether a session's file paths can be settled now. That
// is true with no cwd (nothing to anchor to, ever), inside a git checkout, or
// in a directory that exists but is not a repository (cwd-relative is then the
// final rule — no clone is going to appear there). Only a directory that no
// longer exists might still come back, e.g. a restored checkout.
func anchorable(git *store.GitResolver, cwd string) bool {
	if cwd == "" || git.Root(cwd) != "" {
		return true
	}
	info, err := os.Stat(cwd)
	return err == nil && info.IsDir()
}

func repairPaths(tx *sql.Tx, git *store.GitResolver, res *Result) error {
	type touch struct {
		id        int64
		path, cwd string
	}
	rows, err := tx.Query(`
		SELECT ft.id, ft.path, COALESCE(s.cwd, '')
		  FROM file_touches ft
		  JOIN sessions s ON s.id = ft.session_id
		 WHERE ft.rooted = 0`)
	if err != nil {
		return fmt.Errorf("list file_touches: %w", err)
	}
	var touches []touch
	for rows.Next() {
		var t touch
		if err := rows.Scan(&t.id, &t.path, &t.cwd); err != nil {
			rows.Close()
			return err
		}
		touches = append(touches, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, t := range touches {
		if !anchorable(git, t.cwd) {
			res.TouchesPending++
			continue
		}
		newPath := git.ProjectPath(t.cwd, t.path)
		if _, err := tx.Exec(`UPDATE file_touches SET path = ?, rooted = 1 WHERE id = ?`, newPath, t.id); err != nil {
			return fmt.Errorf("update file_touch %d: %w", t.id, err)
		}
		res.TouchesRooted++
		if newPath != t.path {
			res.TouchesRewritten++
		}
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// TouchesResult counts what a Touches run found and added.
type TouchesResult struct {
	Messages int // apply_patch calls with no file_touches yet
	Added    int // file_touches rows created
	Empty    int // of those calls, ones whose patch named no files
	Pending  int // rows added un-rooted because their directory is gone; `repair git` settles them later
}

// Touches backfills file_touches for Codex apply_patch calls ingested before
// kae knew how to read them. The patch text is already in messages.content,
// so no log file is needed. A call that already has touches is skipped, which
// is what makes it safe to repeat. Like Git it runs in one transaction.
func Touches(db *sql.DB, git *store.GitResolver, dryRun bool) (TouchesResult, error) {
	var res TouchesResult
	tx, err := db.Begin()
	if err != nil {
		return res, fmt.Errorf("begin repair: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT m.id, m.session_id, COALESCE(m.created_at, ''), m.content, COALESCE(s.cwd, ''), s.project_id
		  FROM messages m
		  JOIN sessions s ON s.id = m.session_id
		 WHERE m.kind = 'tool_call' AND m.tool_name = 'apply_patch'
		   AND NOT EXISTS (SELECT 1 FROM file_touches f WHERE f.message_id = m.id)
		 ORDER BY m.id`)
	if err != nil {
		return res, fmt.Errorf("list apply_patch calls: %w", err)
	}
	type call struct {
		id                      int64
		session, at, patch, cwd string
		project                 sql.NullInt64
	}
	var calls []call
	for rows.Next() {
		var c call
		if err := rows.Scan(&c.id, &c.session, &c.at, &c.patch, &c.cwd, &c.project); err != nil {
			rows.Close()
			return res, err
		}
		calls = append(calls, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, err
	}
	rows.Close()

	for _, c := range calls {
		res.Messages++
		touches := codex.PatchFileTouches("apply_patch", c.patch)
		if len(touches) == 0 {
			res.Empty++
			continue
		}
		rooted := 1
		if !anchorable(git, c.cwd) {
			rooted = 0
			res.Pending += len(touches)
		}
		for _, ft := range touches {
			if _, err := tx.Exec(`INSERT INTO file_touches(session_id, message_id, project_id, path, action, created_at, rooted)
			                       VALUES (?,?,?,?,?,?,?)`,
				c.session, c.id, c.project, git.ProjectPath(c.cwd, ft.Path), ft.Action, nullIfEmpty(c.at), rooted); err != nil {
				return res, fmt.Errorf("insert file_touch for message %d: %w", c.id, err)
			}
			res.Added++
		}
	}
	if dryRun {
		return res, nil
	}
	return res, tx.Commit()
}
