// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/danPoku/kaectx/internal/redact"
	"github.com/danPoku/kaectx/internal/store"
)

// upsertSession creates sessionID's row on first sight and otherwise
// updates only the fields this line actually carries new information for.
// msg is the conversational message this update rode in on, if any — used
// purely to seed the title fallback ("first user prompt, trimmed") while
// the session has no title yet.
//
// The fallback must be able to fill in on UPDATE, not only on INSERT. A
// Codex rollout opens with a session_meta line that carries no message, so
// the row is created untitled and the first prompt only arrives on a later
// line; filling the title only at INSERT left every Codex session untitled.
// An agent-provided title (Claude Code's ai-title) still wins whenever it
// arrives, because it is applied first in the COALESCE.
//
// It returns the session's project_id (nil if unresolved) so callers that
// need it — recording a file_touches row alongside a tool_call message —
// don't have to re-query it themselves on every message.
func upsertSession(db *sql.DB, sessionID, agent string, upd SessionUpdate, msg *RawMessage) (any, error) {
	// Titles are shown in every listing and are as likely as a prompt to
	// contain a pasted key, so they get the same redaction as content.
	// Redact before truncating: cutting first could split a key in half
	// and leave a fragment the patterns no longer recognize.
	upd.Title = redact.Redact(upd.Title)
	fallbackTitle := ""
	if msg != nil && msg.Role == "user" && msg.Kind == "text" {
		fallbackTitle = TitleFromPrompt(msg.Content)
	}

	var projectID sql.NullInt64
	err := db.QueryRow(`SELECT project_id FROM sessions WHERE id = ?`, sessionID).Scan(&projectID)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		_, err := db.Exec(`UPDATE sessions SET
		                       cwd        = COALESCE(NULLIF(?, ''), cwd),
		                       git_branch = COALESCE(NULLIF(?, ''), git_branch),
		                       starting_commit = COALESCE(starting_commit, NULLIF(?, '')),
		                       ending_commit = COALESCE(NULLIF(?, ''), ending_commit),
		                       -- A span is only as trustworthy as its weakest end, so the
		                       -- source can worsen as lines arrive but never improve.
		                       commit_source = CASE
		                           WHEN ? = '' THEN commit_source
		                           WHEN commit_source IS NULL THEN ?
		                           WHEN `+sourceRankSQL("?")+` > `+sourceRankSQL("commit_source")+` THEN ?
		                           ELSE commit_source END,
		                       model      = COALESCE(NULLIF(?, ''), model),
		                       title      = COALESCE(NULLIF(?, ''), title, NULLIF(?, '')),
		                       ended_at   = CASE WHEN ? > COALESCE(ended_at, '') THEN ? ELSE ended_at END
		                     WHERE id = ?`,
			upd.CWD, upd.GitBranch, upd.GitCommit, upd.GitCommit,
			upd.CommitSource, upd.CommitSource, upd.CommitSource, upd.CommitSource,
			upd.Model, upd.Title, fallbackTitle, upd.Timestamp, upd.Timestamp, sessionID)
		return nullInt64ToAny(projectID), err
	}

	// project_id stays NULL if we've never seen a cwd for this session —
	// shouldn't happen with real Claude Code logs, but resolving "" would
	// otherwise create a bogus project named ".".
	var newProjectID any
	if upd.CWD != "" {
		id, err := store.ResolveProject(db, upd.CWD)
		if err != nil {
			return nil, err
		}
		newProjectID = id
	}

	title := upd.Title
	if title == "" {
		title = fallbackTitle
	}

	_, err = db.Exec(`INSERT INTO sessions(id, agent, native_id, project_id, cwd, git_branch, starting_commit, ending_commit, commit_source, model, started_at, ended_at, title)
	                   VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sessionID, agent, upd.NativeID, newProjectID,
		nullIfEmpty(upd.CWD), nullIfEmpty(upd.GitBranch), nullIfEmpty(upd.GitCommit), nullIfEmpty(upd.GitCommit),
		nullIfEmpty(upd.CommitSource), nullIfEmpty(upd.Model),
		upd.Timestamp, upd.Timestamp, nullIfEmpty(title))
	return newProjectID, err
}

func nullInt64ToAny(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

// TitleFromPrompt is the fallback session title: the first user prompt,
// redacted, whitespace collapsed onto one line, cut to 80 characters.
func TitleFromPrompt(prompt string) string {
	return truncate(strings.Join(strings.Fields(redact.Redact(prompt)), " "), 80)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// sourceRankSQL renders store.SourceRank as a SQL expression over expr, so
// "which source is weaker" is decided by the one Go table instead of a copy
// of it that could drift.
func sourceRankSQL(expr string) string {
	out := "CASE " + expr
	for src, rank := range store.SourceRank {
		if src != "" {
			out += fmt.Sprintf(" WHEN '%s' THEN %d", src, rank)
		}
	}
	return out + " ELSE 0 END"
}
