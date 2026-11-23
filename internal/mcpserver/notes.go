// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/store"
	"github.com/danPoku/kaectx/queries"
)

// validNoteKinds mirrors notes.kind's CHECK constraint in
// migrations/001_core.sql — validated here too so a bad kind comes back as
// a clear tool error instead of an opaque SQLite constraint failure.
var validNoteKinds = map[string]bool{
	"decision": true, "gotcha": true, "convention": true, "todo": true, "fact": true,
}

type saveNoteArgs struct {
	Kind  string   `json:"kind" jsonschema:"one of: decision, gotcha, convention, todo, fact"`
	Title string   `json:"title" jsonschema:"short title for this note"`
	Body  string   `json:"body,omitempty" jsonschema:"the note's full content"`
	Tags  []string `json:"tags,omitempty" jsonschema:"free-form tags for filtering later"`
	// Supersedes is 0 when absent: note IDs start at 1, so 0 can never name
	// a real note and needs no pointer to mean "not given".
	Supersedes int64 `json:"supersedes,omitempty" jsonschema:"ID of an earlier note in this project that this note replaces; that note stops appearing in search_notes"`
}

type saveNoteResult struct {
	ID int64 `json:"id"`
	// Superseded echoes the retired note's ID, so the caller can see the
	// retirement happened rather than infer it from the absence of an error.
	Superseded int64 `json:"superseded,omitempty"`
}

func (s *server) registerNotes(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "save_note",
		Description: "Record a deliberate note for this project — a decision, gotcha, convention, todo, or fact worth remembering across sessions and agents. " +
			"For things worth writing down on purpose, not a transcript of what happened (that's captured automatically by ingest). " +
			"When this note replaces an earlier one (a reversed decision, a newer handoff), pass that note's ID as supersedes: search_notes then returns only the current note.",
	}, s.saveNote)
}

func (s *server) saveNote(_ context.Context, req *mcp.CallToolRequest, args saveNoteArgs) (*mcp.CallToolResult, saveNoteResult, error) {
	if !validNoteKinds[args.Kind] {
		return nil, saveNoteResult{}, fmt.Errorf("kind must be one of decision, gotcha, convention, todo, fact — got %q", args.Kind)
	}
	if args.Title == "" {
		return nil, saveNoteResult{}, fmt.Errorf("title is required")
	}
	if args.Supersedes < 0 {
		return nil, saveNoteResult{}, fmt.Errorf("supersedes must be a note ID — got %d", args.Supersedes)
	}

	agent := "unknown"
	if ci := req.ClientInfo(); ci != nil && ci.Name != "" {
		agent = ci.Name
	}

	tags := args.Tags
	if tags == nil {
		tags = []string{}
	}
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return nil, saveNoteResult{}, err
	}

	sqlText, err := store.LoadQuery(queries.FS, "notes.sql", "SaveNote")
	if err != nil {
		return nil, saveNoteResult{}, err
	}

	// One transaction for the insert and the retirement, like an amended
	// policy clause: the new clause and the strike-through on the old one are
	// filed together or not at all. A rejected supersedes must leave no new
	// note behind, and a crash between the two writes must not leave both
	// notes live.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, saveNoteResult{}, err
	}
	defer tx.Rollback() // no-op after Commit

	if args.Supersedes != 0 {
		if err := s.checkSupersedable(tx, args.Supersedes); err != nil {
			return nil, saveNoteResult{}, err
		}
	}

	// session_id is left NULL: an MCP tool call isn't necessarily tied to
	// a session ctx has already ingested (that session may still be live,
	// or belong to an agent whose adapter hasn't run yet), so there's no
	// reliable id to attach here without guessing.
	res, err := tx.Exec(sqlText,
		sql.Named("project_id", s.projectID), sql.Named("session_id", nil), sql.Named("agent", agent),
		sql.Named("kind", args.Kind), sql.Named("title", args.Title), sql.Named("body", args.Body),
		sql.Named("tags", string(tagsJSON)))
	if err != nil {
		return nil, saveNoteResult{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, saveNoteResult{}, err
	}

	if args.Supersedes != 0 {
		retire, err := store.LoadQuery(queries.FS, "notes.sql", "SupersedeNote")
		if err != nil {
			return nil, saveNoteResult{}, err
		}
		res, err := tx.Exec(retire,
			sql.Named("superseded_by", id), sql.Named("id", args.Supersedes), sql.Named("project_id", s.projectID))
		if err != nil {
			return nil, saveNoteResult{}, err
		}
		// The check above passed, so zero rows means another process retired
		// the note between the check and this write. Roll back rather than
		// leave two notes each claiming to be the successor.
		if n, err := res.RowsAffected(); err != nil {
			return nil, saveNoteResult{}, err
		} else if n != 1 {
			return nil, saveNoteResult{}, fmt.Errorf("note %d was superseded while this note was being saved; search_notes for the current one and retry", args.Supersedes)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, saveNoteResult{}, err
	}
	return nil, saveNoteResult{ID: id, Superseded: args.Supersedes}, nil
}

// checkSupersedable rejects a supersedes ID that must not be retired: one that
// doesn't exist, one that isn't this project's, and one that has already been
// replaced. The last error names the live successor, because that is the note
// the caller almost certainly meant.
func (s *server) checkSupersedable(tx *sql.Tx, noteID int64) error {
	sqlText, err := store.LoadQuery(queries.FS, "notes.sql", "NoteSupersedeState")
	if err != nil {
		return err
	}
	var projectID, supersededBy sql.NullInt64
	err = tx.QueryRow(sqlText, sql.Named("id", noteID)).Scan(&projectID, &supersededBy)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("supersedes: note %d not found", noteID)
	}
	if err != nil {
		return err
	}
	// A global note (project_id NULL) is shared by every project, so no one
	// project's agent may retire it.
	if !projectID.Valid || projectID.Int64 != s.projectID {
		return fmt.Errorf("supersedes: note %d belongs to another project", noteID)
	}
	if !supersededBy.Valid {
		return nil
	}
	// Walk the chain A -> B -> C to its live end. The step cap only guards
	// against a cycle written by hand; save_note itself can never create one,
	// because a note is always retired by a note newer than it.
	live := supersededBy.Int64
	for range 1000 {
		var next sql.NullInt64
		if err := tx.QueryRow(sqlText, sql.Named("id", live)).Scan(&projectID, &next); err != nil || !next.Valid {
			break
		}
		live = next.Int64
	}
	return fmt.Errorf("supersedes: note %d is already superseded; its live successor is note %d", noteID, live)
}
