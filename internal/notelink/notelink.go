// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package notelink ties a saved note back to the conversation it was written
// in. save_note cannot do this itself: when the tool is called the session is
// usually still live and not yet ingested, so there is no session row to
// point at. But the agent's own log records the call and its result, so once
// that log is ingested the link can be read off the stored messages.
//
// Like a filing clerk who stamps a letter "re: your call of the 4th" only
// after the call log for the 4th has arrived: the letter was written first,
// and the reference is added when the record it refers to exists.
//
// The link is read from messages, not from raw log lines, so the same code
// serves a fresh ingest and a backfill of sessions ingested long ago.
package notelink

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/danPoku/kaectx/internal/redact"
)

// saveNoteTool matches the name Claude Code logs for ctx's save_note tool:
// mcp__<server>__save_note, where <server> is whatever name the user
// registered ctx under (usually "ctx"). Another MCP server could expose a
// tool with the same name; the ID-and-title check below is what stops its
// calls claiming a ctx note.
//
// Codex is not matched. Codex reaches MCP tools through a script it passes to
// its "exec" tool, so a save_note call is JavaScript inside another tool's
// input, and no Codex log with one existed to learn the exact shape from.
const saveNoteTool = `mcp\_\_%\_\_save\_note`

// DB is what linking needs; *sql.DB and *sql.Tx both satisfy it.
type DB interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

// Result counts what a linking pass found.
type Result struct {
	// Calls is the number of save_note results read that named a note ID.
	Calls int
	// Linked is the number of notes given a source by this pass.
	Linked int
	// AlreadyLinked is the number of calls whose note already had a source.
	AlreadyLinked int
	// Unmatched is the number of calls that named an ID but matched no note:
	// the ID is unknown here, the title differs, or no call with that title
	// precedes the result. A log copied from another machine's catalogue
	// produces these, and they are left alone.
	Unmatched int
}

func (r *Result) add(o Result) {
	r.Calls += o.Calls
	r.Linked += o.Linked
	r.AlreadyLinked += o.AlreadyLinked
	r.Unmatched += o.Unmatched
}

// call is one save_note tool call found in a session.
type call struct {
	seq   int
	title string
}

// LinkSession links every note whose save_note call is in sessionID's stored
// messages. It is safe to run any number of times: a note that already has a
// source is never changed, so re-ingesting a log changes nothing.
func LinkSession(db DB, sessionID string) (Result, error) {
	var res Result
	rows, err := db.Query(`SELECT seq, kind, content FROM messages
		WHERE session_id = ? AND tool_name LIKE ? ESCAPE '\' AND kind IN ('tool_call', 'tool_result')
		ORDER BY seq`, sessionID, saveNoteTool)
	if err != nil {
		return res, err
	}
	type result struct {
		seq int
		id  int64
	}
	var calls []call
	var results []result
	for rows.Next() {
		var seq int
		var kind, content string
		if err := rows.Scan(&seq, &kind, &content); err != nil {
			rows.Close()
			return res, err
		}
		if kind == "tool_call" {
			if title, ok := callTitle(content); ok {
				calls = append(calls, call{seq: seq, title: title})
			}
			continue
		}
		// A failed call's result is an error message, not {"id": N}; it and
		// anything else malformed is skipped. Never crash on a bad line.
		if id, ok := resultNoteID(content); ok {
			results = append(results, result{seq: seq, id: id})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, err
	}
	rows.Close()

	for _, r := range results {
		res.Calls++
		var title string
		var linkedSession sql.NullString
		err := db.QueryRow(`SELECT title, session_id FROM notes WHERE id = ?`, r.id).Scan(&title, &linkedSession)
		if errors.Is(err, sql.ErrNoRows) {
			res.Unmatched++
			continue
		}
		if err != nil {
			return res, err
		}
		if linkedSession.Valid {
			res.AlreadyLinked++
			continue
		}
		// The ID alone is not enough: note 13 exists in every catalogue that
		// has saved thirteen notes. The call must also have carried this
		// note's title. The logged title went through redaction on its way
		// into messages, so the note's title is redacted the same way before
		// the two are compared.
		callSeq, ok := precedingCall(calls, r.seq, redact.Redact(title))
		if !ok {
			res.Unmatched++
			continue
		}
		anchor, err := exchangeStart(db, sessionID, callSeq)
		if err != nil {
			return res, err
		}
		res.Linked++
		// The session_id IS NULL guard keeps this a no-op if another process
		// linked the note between the read above and this write.
		if _, err := db.Exec(`UPDATE notes SET session_id = ?, anchor_seq = ?
			WHERE id = ? AND session_id IS NULL`, sessionID, anchor, r.id); err != nil {
			return res, err
		}
	}
	return res, nil
}

// callTitle reads the title from a save_note call's logged input.
func callTitle(content string) (string, bool) {
	var in struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(content), &in); err != nil || in.Title == "" {
		return "", false
	}
	return in.Title, true
}

// resultNoteID reads the note ID from a save_note result: {"id": N}, with
// "superseded" beside it when the call retired an older note.
func resultNoteID(content string) (int64, bool) {
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(content), &out); err != nil || out.ID <= 0 {
		return 0, false
	}
	return out.ID, true
}

// precedingCall finds the save_note call a result answers: the nearest call
// before it with the note's title. Nearest, because one session can save the
// same title twice (a handoff rewritten), and each result belongs to the call
// just before it.
func precedingCall(calls []call, resultSeq int, title string) (int, bool) {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].seq < resultSeq && calls[i].title == title {
			return calls[i].seq, true
		}
	}
	return 0, false
}

// exchangeStart is the first message of the exchange holding callSeq: the
// user turn the assistant was answering when it saved the note. That is the
// same boundary the chunker cuts on, so the anchor lands at the start of the
// chunk that contains the call. A call with no user turn before it (a session
// that opens with tool use) anchors on itself.
func exchangeStart(db DB, sessionID string, callSeq int) (int, error) {
	var start sql.NullInt64
	err := db.QueryRow(`SELECT MAX(seq) FROM messages
		WHERE session_id = ? AND seq <= ? AND role = 'user' AND kind = 'text'`, sessionID, callSeq).Scan(&start)
	if err != nil {
		return 0, err
	}
	if !start.Valid {
		return callSeq, nil
	}
	return int(start.Int64), nil
}

// Backfill links notes saved before linking existed, from the messages
// already stored. It visits every session that holds a save_note result, in
// the order the sessions started, so when two sessions could claim one note
// (a resumed log, say) the earlier one, the one that really saved it, wins.
//
// A dry run does the same work inside a transaction and rolls it back, so it
// reports exactly what a real run would do, not an estimate.
func Backfill(db *sql.DB, dryRun bool) (Result, error) {
	var total Result
	rows, err := db.Query(`SELECT s.id FROM sessions s
		WHERE EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id
			AND m.kind = 'tool_result' AND m.tool_name LIKE ? ESCAPE '\')
		ORDER BY s.started_at, s.id`, saveNoteTool)
	if err != nil {
		return total, err
	}
	var sessions []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return total, err
		}
		sessions = append(sessions, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return total, err
	}
	rows.Close()

	tx, err := db.Begin()
	if err != nil {
		return total, err
	}
	defer tx.Rollback() // no-op after Commit
	for _, id := range sessions {
		res, err := LinkSession(tx, id)
		if err != nil {
			return total, err
		}
		total.add(res)
	}
	if dryRun {
		return total, nil
	}
	return total, tx.Commit()
}
