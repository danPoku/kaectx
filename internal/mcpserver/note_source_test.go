// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"fmt"
	"strings"
	"testing"
)

// #46: search_notes returns where a note was saved and the call that opens
// that exchange; a note with no known source returns neither.
func TestSearchNotesReturnsItsSource(t *testing.T) {
	cs, db := testServer(t)
	save := func(title string) int64 {
		t.Helper()
		var out struct {
			ID int64 `json:"id"`
		}
		callTool(t, cs, "save_note", map[string]any{"kind": "decision", "title": title, "body": "sourcecheck"}, &out)
		return out.ID
	}
	chunked := save("Saved in a chunked exchange")
	recent := save("Saved in an exchange not chunked yet")
	unknown := save("Saved before its session was ingested")

	// The fixture session's first exchange is chunked from message 0.
	const session = "claude-code:sess-1"
	var chunkID int64
	var lastSeq int
	if err := db.QueryRow(`SELECT id, last_seq FROM chunks WHERE session_id = ? AND first_seq = 0`, session).Scan(&chunkID, &lastSeq); err != nil {
		t.Fatalf("setup: no chunk starting at message 0: %v", err)
	}
	link := func(id int64, anchor int) {
		t.Helper()
		if _, err := db.Exec(`UPDATE notes SET session_id = ?, anchor_seq = ? WHERE id = ?`, session, anchor, id); err != nil {
			t.Fatal(err)
		}
	}
	link(chunked, 0)
	link(recent, 90000) // past every chunk: the exchange has not been cut yet

	var out struct {
		Notes []struct {
			ID        int64  `json:"id"`
			SessionID string `json:"session_id"`
			AnchorSeq *int   `json:"anchor_seq"`
			NextStep  string `json:"next_step"`
		} `json:"notes"`
	}
	callTool(t, cs, "search_notes", map[string]any{"query": "sourcecheck"}, &out)
	if len(out.Notes) != 3 {
		t.Fatalf("notes = %+v, want all three", out.Notes)
	}
	want := map[int64]struct {
		session string
		anchor  *int
		next    string
	}{
		chunked: {session, intPtr(0), fmt.Sprintf("get_chunk(chunk_id=%d)", chunkID)},
		recent:  {session, intPtr(90000), fmt.Sprintf("get_session(session_id=%q, from_seq=90000)", session)},
		unknown: {"", nil, ""},
	}
	for _, n := range out.Notes {
		w := want[n.ID]
		anchorOK := (n.AnchorSeq == nil) == (w.anchor == nil) && (n.AnchorSeq == nil || *n.AnchorSeq == *w.anchor)
		if n.SessionID != w.session || !anchorOK || n.NextStep != w.next {
			t.Errorf("note %d = session %q, anchor %v, next_step %q; want %q, %v, %q",
				n.ID, n.SessionID, deref(n.AnchorSeq), n.NextStep, w.session, deref(w.anchor), w.next)
		}
	}

	// The next_step really opens the exchange: make the call it names.
	var chunk struct {
		SessionID string `json:"session_id"`
		FirstSeq  int    `json:"first_seq"`
		Text      string `json:"text"`
	}
	callTool(t, cs, "get_chunk", map[string]any{"chunk_id": chunkID}, &chunk)
	if chunk.SessionID != session || chunk.FirstSeq != 0 || !strings.Contains(chunk.Text, "WAL") {
		t.Errorf("get_chunk from next_step = %+v, want the session's first exchange", chunk)
	}
}

func intPtr(n int) *int { return &n }

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
