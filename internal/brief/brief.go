// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package brief builds the short summary of a project's past work that an
// agent is given when a session starts.
//
// Without it, an agent only learns what happened before if it thinks to
// search. The brief is the handover sheet at a shift change: the nurse coming
// on is told the three things that matter and where the full notes are, and
// does not have to guess that there is something to ask about. It is short on
// purpose (a hard budget, stated in the text), it says nothing when there is
// nothing to say, and every line points at the call that opens the source.
//
// It contains no model output: every line is a stored title, note, path or
// date. Extracted decisions and failures (#68) are a later addition.
package brief

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultBudget is the brief's size limit in tokens when none is given.
const DefaultBudget = 600

// charsPerToken converts the budget to characters. Four characters to a
// token is the usual rule of thumb for English and code; the brief does not
// need a tokenizer to stay comfortably within a few hundred tokens, only a
// rule it never breaks.
const charsPerToken = 4

// HandoffTag marks a note as a handoff. Notes have no "handoff" kind (the
// kinds are fixed by a CHECK constraint in the first migration), so a tag is
// how `kae handoff` labels the notes it writes. A note is also a handoff
// when a checkpoint recorded it as one.
const HandoffTag = "handoff"

// Handoff is the newest live handoff note of a project.
type Handoff struct {
	NoteID    int64
	Title     string
	Body      string
	Agent     string
	CreatedAt string
	// SessionID and AnchorSeq say where the note was saved, when known.
	SessionID string
	AnchorSeq int
	HasSource bool
}

// Session is one recent session.
type Session struct {
	ID       string
	Agent    string
	Title    string
	At       string // last activity
	Messages int
	Files    []string // files it changed, most-touched first
}

// File is a file with how many times sessions changed it in the window.
type File struct {
	Path    string
	Touches int
}

// Brief is everything that may go into the text, before the budget is
// applied.
type Brief struct {
	Project  string
	Handoff  *Handoff
	Sessions []Session
	Files    []File
}

// Empty reports whether there is nothing to tell.
func (b Brief) Empty() bool {
	return b.Handoff == nil && len(b.Sessions) == 0 && len(b.Files) == 0
}

const (
	maxSessions      = 5
	maxFiles         = 5
	maxSessionFiles  = 3
	recentFileWindow = 7 * 24 * time.Hour
)

// Build reads the brief's material for projectID as of now.
func Build(db *sql.DB, projectID int64, now time.Time) (Brief, error) {
	var b Brief
	if err := db.QueryRow(`SELECT name FROM projects WHERE id = ?`, projectID).Scan(&b.Project); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return b, nil
		}
		return b, err
	}

	h, err := LatestHandoff(db, projectID)
	if err != nil {
		return b, err
	}
	b.Handoff = h

	// Subagent transcripts are left out: they are parts of a session the
	// user never saw as a session of their own.
	rows, err := db.Query(`SELECT id, agent, coalesce(title, ''), coalesce(ended_at, started_at), message_count
		FROM sessions WHERE project_id = ? AND parent_session_id IS NULL
		ORDER BY coalesce(ended_at, started_at) DESC LIMIT ?`, projectID, maxSessions)
	if err != nil {
		return b, err
	}
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.Agent, &s.Title, &s.At, &s.Messages); err != nil {
			rows.Close()
			return b, err
		}
		b.Sessions = append(b.Sessions, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return b, err
	}
	for i := range b.Sessions {
		files, err := stringList(db, `SELECT path FROM file_touches WHERE session_id = ? AND action != 'read'
			GROUP BY path ORDER BY count(*) DESC, path LIMIT ?`, b.Sessions[i].ID, maxSessionFiles)
		if err != nil {
			return b, err
		}
		b.Sessions[i].Files = files
	}

	since := now.Add(-recentFileWindow).UTC().Format(time.RFC3339)
	frows, err := db.Query(`SELECT path, count(*) FROM file_touches
		WHERE project_id = ? AND action != 'read' AND created_at >= ?
		GROUP BY path ORDER BY count(*) DESC, path LIMIT ?`, projectID, since, maxFiles)
	if err != nil {
		return b, err
	}
	defer frows.Close()
	for frows.Next() {
		var f File
		if err := frows.Scan(&f.Path, &f.Touches); err != nil {
			return b, err
		}
		b.Files = append(b.Files, f)
	}
	return b, frows.Err()
}

// LatestHandoff returns the newest live handoff note of a project, or nil.
// A note is a handoff when it carries HandoffTag or when a checkpoint was
// answered with saved_handoff naming it. A superseded note is never
// returned: its replacement is the one to read.
func LatestHandoff(db *sql.DB, projectID int64) (*Handoff, error) {
	var h Handoff
	var agent, session sql.NullString
	var anchor sql.NullInt64
	err := db.QueryRow(`SELECT n.id, n.title, n.body, n.agent, n.created_at, n.session_id, n.anchor_seq
		FROM notes n
		WHERE n.project_id = ? AND n.superseded_by IS NULL
		  AND (EXISTS (SELECT 1 FROM json_each(n.tags) t WHERE t.value = ?)
		    OR EXISTS (SELECT 1 FROM checkpoint_observations co WHERE co.note_id = n.id AND co.outcome = 'saved_handoff'))
		ORDER BY n.created_at DESC, n.id DESC LIMIT 1`, projectID, HandoffTag).
		Scan(&h.NoteID, &h.Title, &h.Body, &agent, &h.CreatedAt, &session, &anchor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h.Agent = agent.String
	if session.Valid && anchor.Valid {
		h.SessionID, h.AnchorSeq, h.HasSource = session.String, int(anchor.Int64), true
	}
	return &h, nil
}

func stringList(db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Rendered is a brief as text, with what it cost and what it carried.
type Rendered struct {
	// Text is empty when there was nothing to tell.
	Text string
	// Tokens is the estimated size of Text, and Budget the limit it was
	// held to.
	Tokens, Budget int
	// HandoffNoteID is the handoff note included in Text, or 0. A handoff
	// that did not fit is not counted as delivered.
	HandoffNoteID int64
}

// Tokens estimates the token count of s.
func Tokens(s string) int {
	return (len(s) + charsPerToken - 1) / charsPerToken
}

func day(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// oneLine collapses whitespace and cuts s to max characters, marking a cut.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return ""
	}
	return strings.TrimSpace(s[:max-1]) + "…"
}

// Render writes the brief within budget tokens. Sections are added in order
// of value (the handoff, then recent sessions, then busy files) and each
// line is added only if it still fits, so the text can stop early but can
// never run over. A project with no history renders as "", not as a header
// with nothing under it.
func (b Brief) Render(budget int) Rendered {
	if budget <= 0 {
		budget = DefaultBudget
	}
	out := Rendered{Budget: budget}
	if b.Empty() {
		return out
	}
	limit := budget * charsPerToken

	// The header states the budget. Its token figure is filled in last, so
	// room is reserved for the longest it can be.
	const headerFmt = "kae brief: %s (about %d of %d tokens)\nPast work in this project, from every agent. Each line gives the kae tool call that opens it.\n"
	reserve := len(fmt.Sprintf(headerFmt, b.Project, budget, budget))
	if reserve >= limit {
		return out // a budget too small to say anything says nothing
	}

	var body strings.Builder
	used := reserve
	add := func(s string) bool {
		if used+len(s) > limit {
			return false
		}
		body.WriteString(s)
		used += len(s)
		return true
	}

	if h := b.Handoff; h != nil {
		by := ""
		if h.Agent != "" {
			by = ", saved by " + h.Agent
		}
		head := fmt.Sprintf("\nLatest handoff (note %d, %s%s): %s\n", h.NoteID, day(h.CreatedAt), by, oneLine(h.Title, 120))
		// The handoff may take up to half the budget; the rest is kept for
		// the sessions and files.
		room := limit/2 - len(head)
		if room > limit-used-len(head) {
			room = limit - used - len(head)
		}
		text := ""
		if h.Body != "" && room > 40 {
			text = "  " + oneLine(h.Body, room-3) + "\n"
		}
		source := ""
		if h.HasSource {
			source = fmt.Sprintf("  saved in: get_session(session_id=%q, from_seq=%d)\n", h.SessionID, h.AnchorSeq)
		}
		if add(head) {
			out.HandoffNoteID = h.NoteID
			add(text)
			add(source)
		}
	}

	if len(b.Sessions) > 0 && add("\nRecent sessions:\n") {
		for _, s := range b.Sessions {
			title := oneLine(s.Title, 80)
			if title == "" {
				title = "(untitled)"
			}
			line := fmt.Sprintf("- %s %s: %s", day(s.At), s.Agent, title)
			if len(s.Files) > 0 {
				line += " [" + strings.Join(s.Files, ", ") + "]"
			}
			line += fmt.Sprintf("  get_session(session_id=%q)\n", s.ID)
			if !add(line) {
				break
			}
		}
	}

	if len(b.Files) > 0 && add("\nMost-changed files, last 7 days:\n") {
		for _, f := range b.Files {
			if !add(fmt.Sprintf("- %s (%d)  explain_file(path=%q)\n", f.Path, f.Touches, f.Path)) {
				break
			}
		}
	}

	if body.Len() == 0 {
		return Rendered{Budget: budget}
	}
	// Two passes, because the header contains its own size.
	text := fmt.Sprintf(headerFmt, b.Project, 0, budget) + body.String()
	text = fmt.Sprintf(headerFmt, b.Project, Tokens(text), budget) + body.String()
	out.Text, out.Tokens = text, Tokens(text)
	return out
}

// RecordDelivery logs that a brief handed noteID to an agent. It writes the
// same row a search_notes call writes when it returns a note, which is what
// `kae doctor` counts as a handoff being picked up: the next session did
// receive it, whether it searched for it or was given it.
func RecordDelivery(db *sql.DB, noteID int64) error {
	if noteID == 0 {
		return nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT INTO note_retrievals(run_id, rank, note_id) VALUES (?, 1, ?)`, hex.EncodeToString(buf), noteID)
	return err
}
