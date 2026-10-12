// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/search"
	"github.com/danPoku/kaectx/internal/store"
)

func TestFormatSearchShowsRunAndRanks(t *testing.T) {
	got := search.BestResult{RunID: "abc123", Results: []search.Result{
		{Agent: "codex", SessionID: "codex:s", FirstSeq: 1, LastSeq: 2, Snippet: "first"},
		{Agent: "claude-code", SessionID: "claude-code:t", FirstSeq: 3, LastSeq: 4, Snippet: "second"},
	}}
	out := formatSearch(got)
	for _, want := range []string{"run abc123", "ctx feedback abc123 <rank> <outcome>", "#1 [codex]", "#2 [claude-code]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// Without a recorded run there is nothing to label, so no run line and
	// no ranks that would suggest otherwise.
	got.RunID = ""
	out = formatSearch(got)
	if strings.Contains(out, "run ") || strings.Contains(out, "ctx feedback") || strings.Contains(out, "#1") {
		t.Errorf("output without a run ID still offers labelling:\n%s", out)
	}
	if !strings.Contains(out, "[codex]") {
		t.Errorf("output lost its hits:\n%s", out)
	}

	// A run ID beside a telemetry error names a run that was never stored.
	got.RunID, got.TelemetryErr = "abc123", errors.New("commit failed")
	out = formatSearch(got)
	if strings.Contains(out, "abc123") || strings.Contains(out, "#1") {
		t.Errorf("output offers labelling for an unrecorded run:\n%s", out)
	}
}

// feedbackTestDB holds one recorded search with two results in project 1.
func feedbackTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects(id, name) VALUES (1, 'p'), (2, 'other');
		INSERT INTO retrieval_runs(id, project_id, surface, query, requested_limit, mode, duration_ms, result_count)
			VALUES ('run1', 1, 'cli', 'q', 10, 'keyword', 0, 2), ('run2', 2, 'cli', 'q', 10, 'keyword', 0, 1);
		INSERT INTO retrieval_candidates(run_id, rank, evidence_key, session_id, first_seq, last_seq, score, score_kind)
			VALUES ('run1', 1, 'k1', 's', 0, 1, 0, 'bm25'), ('run1', 2, 'k2', 's', 2, 3, 0, 'bm25'),
			       ('run2', 1, 'k3', 's2', 0, 1, 0, 'bm25');`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestFeedbackRecordsAndReplaces(t *testing.T) {
	db := feedbackTestDB(t)
	var out bytes.Buffer
	if err := feedbackIn(db, 1, []string{"run1", "1", "helpful"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "recorded helpful for run1 #1") {
		t.Errorf("output = %q", out.String())
	}
	// A second label for the same result replaces the first.
	out.Reset()
	if err := feedbackIn(db, 1, []string{"--note", "token ghp_" + strings.Repeat("A", 36), "run1", "1", "stale"}, &out); err != nil {
		t.Fatal(err)
	}
	var n int
	var outcome, note string
	var anchor int
	if err := db.QueryRow(`SELECT COUNT(*), MAX(outcome), MAX(note), MAX(anchor_seq) FROM memory_observations WHERE run_id = 'run1'`).Scan(&n, &outcome, &note, &anchor); err != nil {
		t.Fatal(err)
	}
	if n != 1 || outcome != "stale" || anchor != 0 {
		t.Errorf("observations = %d, last outcome %q, anchor %d; want 1 stale anchored at 0", n, outcome, anchor)
	}
	if strings.Contains(note, "ghp_") || !strings.Contains(note, "[REDACTED]") {
		t.Errorf("note stored unredacted: %q", note)
	}
}

func TestFeedbackRejectsBadInputWithoutWriting(t *testing.T) {
	db := feedbackTestDB(t)
	for _, args := range [][]string{
		{"nope", "1", "helpful"}, // unknown run
		{"run1", "9", "helpful"}, // rank not in that run
		{"run2", "1", "helpful"}, // another project's run
		{"run1", "1", "great"},   // invalid outcome
		{"run1", "x", "helpful"}, // rank not a number
		{"run1", "1"},            // missing outcome
		{"run1", "1", "helpful", "extra"},
	} {
		if err := feedbackIn(db, 1, args, &bytes.Buffer{}); err == nil {
			t.Errorf("feedback %v succeeded, want an error", args)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_observations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d observations written by rejected calls, want 0", n)
	}
}
