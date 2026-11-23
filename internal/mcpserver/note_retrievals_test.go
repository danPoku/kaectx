// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type noteSearchOut struct {
	RunID string `json:"run_id"`
	Notes []struct {
		ID int64 `json:"id"`
	} `json:"notes"`
}

func saveHandoff(t *testing.T, cs *sdkmcp.ClientSession, title string) int64 {
	t.Helper()
	var out struct {
		ID int64 `json:"id"`
	}
	callTool(t, cs, "save_note", map[string]any{"kind": "todo", "title": title, "body": "handoff body"}, &out)
	if out.ID == 0 {
		t.Fatal("save_note returned id=0")
	}
	return out.ID
}

// #42: a search_notes call writes one row per returned note, ranks in order.
func TestSearchNotesRecordsWhatItReturned(t *testing.T) {
	cs, db := testServer(t)
	saveHandoff(t, cs, "handoff one")
	saveHandoff(t, cs, "handoff two")
	saveHandoff(t, cs, "handoff three")

	var first, second, none noteSearchOut
	callTool(t, cs, "search_notes", map[string]any{"query": "handoff"}, &first)
	callTool(t, cs, "search_notes", map[string]any{"query": "handoff", "limit": 2}, &second)
	callTool(t, cs, "search_notes", map[string]any{"query": "zebra"}, &none)

	if len(first.Notes) != 3 || len(second.Notes) != 2 {
		t.Fatalf("setup: searches returned %d and %d notes, want 3 and 2", len(first.Notes), len(second.Notes))
	}
	if first.RunID == "" || second.RunID == "" || first.RunID == second.RunID {
		t.Fatalf("run ids = %q, %q; want two distinct ids", first.RunID, second.RunID)
	}
	// Nothing returned, nothing to record, no id to hand out.
	if len(none.Notes) != 0 || none.RunID != "" {
		t.Errorf("empty search = %+v, want no notes and no run_id", none)
	}

	for _, search := range []noteSearchOut{first, second} {
		rows, err := db.Query(`SELECT rank, note_id FROM note_retrievals WHERE run_id = ? ORDER BY rank`, search.RunID)
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		wantRank := 1
		for rows.Next() {
			var rank int
			var noteID int64
			if err := rows.Scan(&rank, &noteID); err != nil {
				t.Fatal(err)
			}
			if rank != wantRank {
				t.Errorf("run %s: rank %d follows rank %d", search.RunID, rank, wantRank-1)
			}
			wantRank++
			got = append(got, noteID)
		}
		rows.Close()
		if len(got) != len(search.Notes) {
			t.Fatalf("run %s wrote %d rows, want one per returned note (%d)", search.RunID, len(got), len(search.Notes))
		}
		for i, n := range search.Notes {
			if got[i] != n.ID {
				t.Errorf("run %s rank %d is note %d, want %d (the order returned)", search.RunID, i+1, got[i], n.ID)
			}
		}
	}
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM note_retrievals`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Errorf("note_retrievals has %d rows, want 5 (3 + 2 + 0)", total)
	}
}

// #42: losing the telemetry must never lose the search.
func TestSearchNotesSurvivesTelemetryFailure(t *testing.T) {
	cs, db := testServer(t)
	id := saveHandoff(t, cs, "handoff one")
	if _, err := db.Exec(`DROP TABLE note_retrievals`); err != nil {
		t.Fatal(err)
	}
	var out noteSearchOut
	callTool(t, cs, "search_notes", map[string]any{"query": "handoff"}, &out)
	if len(out.Notes) != 1 || out.Notes[0].ID != id {
		t.Fatalf("notes = %+v, want the saved note despite the failed record", out.Notes)
	}
	if out.RunID != "" {
		t.Errorf("run_id = %q, want none: nothing was recorded under it", out.RunID)
	}
}

// recommendCheckpoint puts the fixture session over the message threshold and
// records the recommendation, returning the session ID to answer it with.
func recommendCheckpoint(t *testing.T, cs *sdkmcp.ClientSession, db *sql.DB) string {
	t.Helper()
	if _, err := db.Exec(`UPDATE sessions SET message_count = 400`); err != nil {
		t.Fatal(err)
	}
	var check checkpointOut
	callTool(t, cs, "checkpoint_status", map[string]any{}, &check)
	if !check.ShouldSignal {
		t.Fatalf("setup: checkpoint = %+v, want a recommendation", check)
	}
	return check.SessionID
}

func TestRecordCheckpointOutcomeLinksTheHandoffNote(t *testing.T) {
	cs, db := testServer(t)
	sessionID := recommendCheckpoint(t, cs, db)
	noteID := saveHandoff(t, cs, "handoff one")

	var out struct {
		Recorded bool  `json:"recorded"`
		NoteID   int64 `json:"note_id"`
	}
	callTool(t, cs, "record_checkpoint_outcome", map[string]any{
		"session_id": sessionID, "outcome": "saved_handoff", "note_id": noteID}, &out)
	if !out.Recorded || out.NoteID != noteID {
		t.Fatalf("result = %+v, want recorded with note_id %d", out, noteID)
	}
	var stored sql.NullInt64
	if err := db.QueryRow(`SELECT note_id FROM checkpoint_observations WHERE session_id = ?`, sessionID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Valid || stored.Int64 != noteID {
		t.Errorf("stored note_id = %+v, want %d", stored, noteID)
	}

	// Deleting the note keeps the outcome and drops only the link.
	if _, err := db.Exec(`DELETE FROM notes WHERE id = ?`, noteID); err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err := db.QueryRow(`SELECT outcome, note_id FROM checkpoint_observations WHERE session_id = ?`, sessionID).Scan(&outcome, &stored); err != nil {
		t.Fatal(err)
	}
	if outcome != "saved_handoff" || stored.Valid {
		t.Errorf("after deleting the note: outcome = %q, note_id = %+v; want saved_handoff and NULL", outcome, stored)
	}
}

// #42: a note_id that is not this project's own handoff is refused, and the
// checkpoint is left unanswered rather than half-recorded.
func TestRecordCheckpointOutcomeRejectsBadNoteID(t *testing.T) {
	cs, db := testServer(t)
	sessionID := recommendCheckpoint(t, cs, db)
	own := saveHandoff(t, cs, "handoff one")

	res, err := db.Exec(`INSERT INTO projects(name) VALUES ('elsewhere')`)
	if err != nil {
		t.Fatal(err)
	}
	otherProject, _ := res.LastInsertId()
	res, err = db.Exec(`INSERT INTO notes(project_id, kind, title) VALUES (?, 'todo', 'their handoff')`, otherProject)
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := res.LastInsertId()
	res, err = db.Exec(`INSERT INTO notes(project_id, kind, title) VALUES (NULL, 'todo', 'global note')`)
	if err != nil {
		t.Fatal(err)
	}
	global, _ := res.LastInsertId()

	cases := []struct {
		name      string
		outcome   string
		noteID    int64
		wantInErr string
	}{
		{"another project's note", "saved_handoff", foreign, "another project"},
		{"global note", "saved_handoff", global, "another project"},
		{"missing note", "saved_handoff", 99999, "not found"},
		{"note_id with an outcome that saved nothing", "dismissed", own, "only applies to saved_handoff"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{
				Name:      "record_checkpoint_outcome",
				Arguments: map[string]any{"session_id": sessionID, "outcome": c.outcome, "note_id": c.noteID},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError {
				t.Fatalf("record_checkpoint_outcome(note_id=%d) succeeded, want a tool error", c.noteID)
			}
			if tc, ok := res.Content[0].(*sdkmcp.TextContent); !ok || !strings.Contains(tc.Text, c.wantInErr) {
				t.Errorf("error = %+v, want it to contain %q", res.Content[0], c.wantInErr)
			}
			var outcome sql.NullString
			var noteID sql.NullInt64
			if err := db.QueryRow(`SELECT outcome, note_id FROM checkpoint_observations WHERE session_id = ?`, sessionID).Scan(&outcome, &noteID); err != nil {
				t.Fatal(err)
			}
			if outcome.Valid || noteID.Valid {
				t.Errorf("a rejected call wrote outcome = %+v, note_id = %+v", outcome, noteID)
			}
		})
	}
}
