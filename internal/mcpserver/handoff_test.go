// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/doctor"
	"github.com/danPoku/kaectx/internal/handoff"
	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/store"
)

type handoffOut struct {
	Found     bool   `json:"found"`
	NoteID    int64  `json:"note_id"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	Agent     string `json:"agent"`
	SessionID string `json:"session_id"`
	AnchorSeq *int   `json:"anchor_seq"`
	NextStep  string `json:"next_step"`
}

// get_handoff returns the handoff `kae handoff` wrote, with the call that
// opens its source, and marks it as picked up in `kae doctor`.
func TestGetHandoff(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, claudecode.New(), filepath.Join("..", "ingest", "claudecode", "testdata", "session.jsonl")); err != nil {
		t.Fatal(err)
	}
	const cwd = "/home/dev/proj"
	projectID, found, err := store.LookupProject(db, cwd)
	if err != nil || !found {
		t.Fatalf("fixture project not found: %v", err)
	}
	// The server is started the way a different agent would start it: same
	// project folder, nothing else shared.
	cs := serverAt(t, db, cwd)

	var out handoffOut
	callTool(t, cs, "get_handoff", map[string]any{}, &out)
	if out.Found || out.Text != "" {
		t.Fatalf("no handoff saved yet, got %+v", out)
	}

	note, err := handoff.Build(db, projectID, "claude-code:sess-1")
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := handoff.Save(db, projectID, note)
	if err != nil {
		t.Fatal(err)
	}

	out = handoffOut{}
	callTool(t, cs, "get_handoff", map[string]any{}, &out)
	if !out.Found || out.NoteID != id || out.Agent != handoff.Author || out.SessionID != "claude-code:sess-1" || out.AnchorSeq == nil {
		t.Fatalf("unexpected handoff: %+v", out)
	}
	if !strings.Contains(out.Text, "Objective (the first prompt):") || !strings.HasPrefix(out.Title, "Handoff: ") {
		t.Errorf("handoff text or title wrong:\n%s", out.Text)
	}
	if want := `get_session(session_id="claude-code:sess-1", from_seq=`; !strings.HasPrefix(out.NextStep, want) {
		t.Errorf("next_step = %q, want it to start %q", out.NextStep, want)
	}

	report, err := doctor.Collect(context.Background(), db, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if report.HandoffsSaved != 1 || report.HandoffsPickedUp != 1 {
		t.Errorf("doctor: %d saved, %d picked up; want 1 and 1", report.HandoffsSaved, report.HandoffsPickedUp)
	}
}
