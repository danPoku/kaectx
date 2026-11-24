// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package handoff writes a handoff note for a session from what kae already
// stores about it, and reads the newest one back.
//
// Until now a handoff existed only if the agent wrote one with save_note when
// a checkpoint suggested it. But the raw material is all on file: what was
// asked for at the start, the last few exchanges, the files changed and the
// commits spanned. `kae handoff` is the clerk who, asked for a handover on a
// case, does not write an opinion but assembles the cover sheet from the
// file: the original request, the latest correspondence, the documents
// altered, and where the full record is.
//
// Nothing in a handoff is written by a model. Every line is quoted or listed
// from stored (and so already redacted) data, and the note says so.
package handoff

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/danPoku/kaectx/internal/brief"
)

// Author is the notes.agent value for handoffs this package writes, which
// tells them apart from handoffs an agent wrote itself.
const Author = "kae"

const (
	objectiveChars = 600
	exchangeChars  = 500
	lastExchanges  = 3
	maxFiles       = 15
)

// Note is a handoff ready to save.
type Note struct {
	SessionID string
	// AnchorSeq is the first message of the session's last exchange, so
	// that opening the note's source lands on where the work stopped.
	AnchorSeq int
	Title     string
	Body      string
	Files     []string
}

// ErrNoSession means the project has no session to hand off.
var ErrNoSession = errors.New("this project has no sessions to hand off; run `kae ingest` after working in it")

// LatestSession returns the most recently active top-level session of a
// project. Subagent transcripts are not sessions the user would hand off.
func LatestSession(db *sql.DB, projectID int64) (string, error) {
	var id string
	err := db.QueryRow(`SELECT id FROM sessions WHERE project_id = ? AND parent_session_id IS NULL
		ORDER BY coalesce(ended_at, started_at) DESC LIMIT 1`, projectID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoSession
	}
	return id, err
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	// Cut on a rune boundary: stored text is UTF-8 and a split character
	// would make the note invalid.
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + " […]"
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

func short(commit string) string {
	if len(commit) > 10 {
		return commit[:10]
	}
	return commit
}

// Build assembles the handoff for sessionID, which must belong to projectID.
func Build(db *sql.DB, projectID int64, sessionID string) (Note, error) {
	var (
		agent, started                  string
		title, branch, from, to, source sql.NullString
		ended                           sql.NullString
		owner                           sql.NullInt64
		messages                        int
	)
	err := db.QueryRow(`SELECT agent, started_at, ended_at, title, git_branch, starting_commit, ending_commit, commit_source, project_id, message_count
		FROM sessions WHERE id = ?`, sessionID).
		Scan(&agent, &started, &ended, &title, &branch, &from, &to, &source, &owner, &messages)
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, fmt.Errorf("session %s not found", sessionID)
	}
	if err != nil {
		return Note{}, err
	}
	if !owner.Valid || owner.Int64 != projectID {
		return Note{}, fmt.Errorf("session %s belongs to another project", sessionID)
	}

	n := Note{SessionID: sessionID}
	name := strings.TrimSpace(title.String)
	if name == "" {
		name = sessionID
	}
	n.Title = "Handoff: " + clip(name, 100)

	var b strings.Builder
	last := started
	if ended.Valid {
		last = ended.String
	}
	fmt.Fprintf(&b, "Handoff for the %s session %s (last active %s, %d messages).\n", agent, sessionID, last, messages)
	b.WriteString("Assembled by `kae handoff` from the stored transcript: quoted and listed, not summarised.\n")

	// The objective is the first thing the person typed. Injected context
	// (IDE notices, environment snapshots) is stored as system events, so
	// the first user text message is the prompt itself.
	var objective string
	err = db.QueryRow(`SELECT content FROM messages WHERE session_id = ? AND role = 'user' AND kind = 'text'
		ORDER BY seq LIMIT 1`, sessionID).Scan(&objective)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Note{}, err
	}
	if strings.TrimSpace(objective) != "" {
		b.WriteString("\nObjective (the first prompt):\n" + indent(clip(objective, objectiveChars)) + "\n")
	}

	// The last exchanges, oldest first, so they read in the order they
	// happened. Chunks are the stored exchanges: one user turn with the
	// assistant's reply, tool noise already trimmed.
	rows, err := db.Query(`SELECT first_seq, text FROM (
			SELECT first_seq, text FROM chunks WHERE session_id = ? ORDER BY first_seq DESC LIMIT ?
		) ORDER BY first_seq`, sessionID, lastExchanges)
	if err != nil {
		return Note{}, err
	}
	var exchanges []string
	for rows.Next() {
		var seq int
		var text string
		if err := rows.Scan(&seq, &text); err != nil {
			rows.Close()
			return Note{}, err
		}
		n.AnchorSeq = seq
		exchanges = append(exchanges, fmt.Sprintf("[from message %d]\n%s", seq, clip(text, exchangeChars)))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Note{}, err
	}
	if len(exchanges) > 0 {
		b.WriteString("\nLast exchanges:\n")
		for _, e := range exchanges {
			b.WriteString(indent(e) + "\n")
		}
	}

	frows, err := db.Query(`SELECT path, group_concat(DISTINCT action), count(*) FROM file_touches
		WHERE session_id = ? AND action != 'read' GROUP BY path ORDER BY count(*) DESC, path LIMIT ?`, sessionID, maxFiles)
	if err != nil {
		return Note{}, err
	}
	var fileLines []string
	for frows.Next() {
		var path, actions string
		var count int
		if err := frows.Scan(&path, &actions, &count); err != nil {
			frows.Close()
			return Note{}, err
		}
		n.Files = append(n.Files, path)
		fileLines = append(fileLines, fmt.Sprintf("  - %s (%s, %d)", path, actions, count))
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		return Note{}, err
	}
	if len(fileLines) > 0 {
		b.WriteString("\nFiles changed:\n" + strings.Join(fileLines, "\n") + "\n")
	}

	if from.String != "" || to.String != "" {
		git := "\nGit: "
		if branch.String != "" {
			git += "branch " + branch.String + ", "
		}
		switch {
		case from.String != "" && to.String != "" && from.String != to.String:
			git += "commits " + short(from.String) + ".." + short(to.String)
		case to.String != "":
			git += "at commit " + short(to.String)
		default:
			git += "from commit " + short(from.String)
		}
		// The same caveat `kae explain` gives: a span not read from the
		// session's own branch is a best guess.
		if source.String == "head" || source.String == "unverified" {
			git += " (approximate)"
		}
		b.WriteString(git + "\n")
	}

	fmt.Fprintf(&b, "\nFull record: get_session(session_id=%q, from_seq=%d)\n", sessionID, n.AnchorSeq)
	n.Body = b.String()
	return n, nil
}

// Save stores n as a handoff note of projectID and returns its id. An
// earlier handoff this package wrote for the same session is retired in the
// same transaction and its id returned as superseded (0 if there was none),
// so a session handed off twice has one current handoff, not two competing.
// Handoffs an agent wrote itself are never retired here.
func Save(db *sql.DB, projectID int64, n Note) (id, superseded int64, err error) {
	tags, err := json.Marshal([]string{brief.HandoffTag})
	if err != nil {
		return 0, 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	err = tx.QueryRow(`SELECT n.id FROM notes n
		WHERE n.project_id = ? AND n.session_id = ? AND n.agent = ? AND n.superseded_by IS NULL
		  AND EXISTS (SELECT 1 FROM json_each(n.tags) t WHERE t.value = ?)
		ORDER BY n.id DESC LIMIT 1`, projectID, n.SessionID, Author, brief.HandoffTag).Scan(&superseded)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, err
	}

	res, err := tx.Exec(`INSERT INTO notes(project_id, session_id, anchor_seq, agent, kind, title, body, tags)
		VALUES (?, ?, ?, ?, 'todo', ?, ?, ?)`, projectID, n.SessionID, n.AnchorSeq, Author, n.Title, n.Body, string(tags))
	if err != nil {
		return 0, 0, err
	}
	if id, err = res.LastInsertId(); err != nil {
		return 0, 0, err
	}
	if superseded != 0 {
		if _, err := tx.Exec(`UPDATE notes SET superseded_by = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')
			WHERE id = ? AND superseded_by IS NULL`, id, superseded); err != nil {
			return 0, 0, err
		}
	}
	return id, superseded, tx.Commit()
}

// Format renders a stored handoff for the terminal or an agent: the note,
// then the call that opens the conversation it came from.
func Format(h *brief.Handoff) string {
	var b strings.Builder
	by := ""
	if h.Agent != "" {
		by = ", written by " + h.Agent
	}
	fmt.Fprintf(&b, "%s\n(note %d, %s%s)\n\n%s", h.Title, h.NoteID, h.CreatedAt, by, strings.TrimRight(h.Body, "\n"))
	b.WriteString("\n")
	// A handoff this package wrote already ends with the call; add it only
	// for one an agent wrote, whose body kae knows nothing about.
	if h.HasSource && !strings.Contains(h.Body, "get_session(") {
		fmt.Fprintf(&b, "\nSaved in: get_session(session_id=%q, from_seq=%d)\n", h.SessionID, h.AnchorSeq)
	}
	return b.String()
}
