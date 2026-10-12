// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"fmt"
	"strings"
	"testing"
)

type checkpointOut struct {
	SessionID      string   `json:"session_id"`
	Level          string   `json:"level"`
	ShouldSignal   bool     `json:"should_signal"`
	Recommendation string   `json:"recommendation"`
	Reasons        []string `json:"reasons"`
	Metrics        struct {
		Messages     int `json:"messages"`
		FilesChanged int `json:"files_changed"`
		DurationMins int `json:"duration_minutes"`
	} `json:"metrics"`
}

type pendingNudge struct {
	Kind       string `json:"kind"`
	Confidence string `json:"confidence"`
	RunID      string `json:"run_id"`
	Rank       int    `json:"rank"`
}

// TestCheckpointStatusListsUnlabelledOpens is the regression test for issue
// #17: checkpoint_status said nothing about opened results still waiting for
// a label, the moment an agent is most likely to have the answer.
func TestCheckpointStatusListsUnlabelledOpens(t *testing.T) {
	cs, db := testServer(t)
	// Over the message threshold, so this call also records a checkpoint
	// recommendation; pending_feedback must not echo that back.
	if _, err := db.Exec(`UPDATE sessions SET message_count = 400,
		started_at = '2026-09-20T10:00:00Z', ended_at = '2026-09-20T11:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	var hit struct {
		RunID   string `json:"run_id"`
		Results []struct {
			ChunkID int64 `json:"chunk_id"`
		} `json:"results"`
	}
	callTool(t, cs, "search_context", map[string]any{"query": "WAL"}, &hit)
	var opened struct{}
	callTool(t, cs, "get_chunk", map[string]any{"chunk_id": hit.Results[0].ChunkID, "run_id": hit.RunID, "rank": 1}, &opened)

	var out struct {
		PendingFeedback []pendingNudge `json:"pending_feedback"`
	}
	callTool(t, cs, "checkpoint_status", map[string]any{}, &out)
	if len(out.PendingFeedback) != 1 {
		t.Fatalf("pending_feedback = %+v, want the one opened result", out.PendingFeedback)
	}
	if p := out.PendingFeedback[0]; p.Kind != "retrieval_outcome_missing" || p.RunID != hit.RunID || p.Rank != 1 || p.Confidence != "high" {
		t.Errorf("pending = %+v, want the exact open at rank 1 with high confidence", p)
	}

	var recorded struct{}
	callTool(t, cs, "record_retrieval_outcome", map[string]any{"run_id": hit.RunID, "rank": 1, "outcome": "helpful"}, &recorded)
	out.PendingFeedback = nil
	callTool(t, cs, "checkpoint_status", map[string]any{}, &out)
	if len(out.PendingFeedback) != 0 {
		t.Errorf("pending_feedback after labelling = %+v, want empty", out.PendingFeedback)
	}

	// Never more than 10, however many results wait.
	for i := 0; i < 12; i++ {
		run := fmt.Sprintf("bulk-%d", i)
		if _, err := db.Exec(`INSERT INTO retrieval_runs(id, project_id, surface, query, requested_limit, mode, duration_ms, result_count)
			SELECT ?, project_id, 'mcp', 'q', 10, 'keyword', 0, 1 FROM retrieval_runs WHERE id = ?`, run, hit.RunID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO retrieval_candidates(run_id, rank, evidence_key, session_id, first_seq, last_seq, score, score_kind, opened_at, open_count)
			VALUES (?, 1, 'k', 's', 0, 1, 0, 'bm25', strftime('%Y-%m-%dT%H:%M:%fZ','now'), 1)`, run); err != nil {
			t.Fatal(err)
		}
	}
	callTool(t, cs, "checkpoint_status", map[string]any{}, &out)
	if len(out.PendingFeedback) != 10 {
		t.Errorf("pending_feedback has %d entries, want the cap of 10", len(out.PendingFeedback))
	}
}

func TestCheckpointStatusHealthySession(t *testing.T) {
	cs, db := testServer(t)
	if _, err := db.Exec(`UPDATE sessions SET message_count = 120,
		started_at = '2026-09-20T10:00:00Z', ended_at = '2026-09-20T11:00:00Z'`); err != nil {
		t.Fatal(err)
	}

	var out checkpointOut
	callTool(t, cs, "checkpoint_status", map[string]any{}, &out)
	if out.ShouldSignal || out.Level != "healthy" {
		t.Fatalf("checkpoint = %+v, want healthy without a signal", out)
	}
	if out.Metrics.Messages != 120 || out.Metrics.DurationMins != 60 {
		t.Errorf("metrics = %+v, want 120 messages and 60 minutes", out.Metrics)
	}
}

func TestCheckpointStatusRecommendsAtOneThreshold(t *testing.T) {
	cs, db := testServer(t)
	if _, err := db.Exec(`UPDATE sessions SET message_count = 400,
		started_at = '2026-09-20T10:00:00Z', ended_at = '2026-09-20T11:00:00Z'`); err != nil {
		t.Fatal(err)
	}

	var out checkpointOut
	callTool(t, cs, "checkpoint_status", map[string]any{}, &out)
	if !out.ShouldSignal || out.Level != "checkpoint_recommended" {
		t.Fatalf("checkpoint = %+v, want checkpoint_recommended", out)
	}
	if len(out.Reasons) != 1 {
		t.Fatalf("reasons = %v, want one threshold reason", out.Reasons)
	}
}

func TestCheckpointStatusStrongForSeveralSignals(t *testing.T) {
	cs, db := testServer(t)
	if _, err := db.Exec(`UPDATE sessions SET message_count = 500,
		started_at = '2026-09-20T10:00:00Z', ended_at = '2026-09-20T15:00:00Z'`); err != nil {
		t.Fatal(err)
	}

	var out checkpointOut
	callTool(t, cs, "checkpoint_status", map[string]any{}, &out)
	if !out.ShouldSignal || out.Level != "checkpoint_strongly_recommended" {
		t.Fatalf("checkpoint = %+v, want checkpoint_strongly_recommended", out)
	}
	if len(out.Reasons) != 2 {
		t.Fatalf("reasons = %v, want message and duration reasons", out.Reasons)
	}
}

func TestCheckpointStatusCanSelectSession(t *testing.T) {
	cs, db := testServer(t)
	var sessionID string
	if err := db.QueryRow(`SELECT id FROM sessions LIMIT 1`).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}

	var out checkpointOut
	callTool(t, cs, "checkpoint_status", map[string]any{"session_id": sessionID}, &out)
	if out.SessionID != sessionID {
		t.Errorf("session_id = %q, want %q", out.SessionID, sessionID)
	}
}

func TestFeedbackStatusTracksCheckpointDecision(t *testing.T) {
	cs, db := testServer(t)
	if _, err := db.Exec(`UPDATE sessions SET message_count=400`); err != nil {
		t.Fatal(err)
	}
	var checkpoint checkpointOut
	callTool(t, cs, "checkpoint_status", map[string]any{}, &checkpoint)

	var inbox struct {
		Pending int `json:"pending"`
		Nudges  []struct {
			Kind      string `json:"kind"`
			SessionID string `json:"session_id"`
		} `json:"nudges"`
	}
	callTool(t, cs, "feedback_status", map[string]any{}, &inbox)
	if inbox.Pending != 1 || inbox.Nudges[0].Kind != "checkpoint_outcome_missing" || inbox.Nudges[0].SessionID != checkpoint.SessionID {
		t.Fatalf("feedback inbox = %+v, want the pending checkpoint", inbox)
	}

	var recorded struct {
		Recorded bool   `json:"recorded"`
		Outcome  string `json:"outcome"`
	}
	callTool(t, cs, "record_checkpoint_outcome", map[string]any{
		"session_id": checkpoint.SessionID, "outcome": "continued", "note": "user chose to keep going",
	}, &recorded)
	if !recorded.Recorded || recorded.Outcome != "continued" {
		t.Fatalf("recorded outcome = %+v", recorded)
	}
	callTool(t, cs, "feedback_status", map[string]any{}, &inbox)
	if inbox.Pending != 0 {
		t.Fatalf("feedback pending = %d after resolution, want 0", inbox.Pending)
	}
}

func TestFeedbackStatusDerivesUnlabelledOpenedResult(t *testing.T) {
	cs, _ := testServer(t)
	var search struct {
		RetrievalRunID string `json:"retrieval_run_id"`
		Results        []struct {
			ChunkID int64 `json:"chunk_id"`
			Rank    int   `json:"rank"`
		} `json:"results"`
	}
	callTool(t, cs, "search_context", map[string]any{"query": "WAL"}, &search)
	var chunk struct{}
	callTool(t, cs, "get_chunk", map[string]any{
		"chunk_id": search.Results[0].ChunkID, "run_id": search.RetrievalRunID, "rank": search.Results[0].Rank,
	}, &chunk)

	var inbox struct {
		Pending int `json:"pending"`
		Nudges  []struct {
			Kind       string `json:"kind"`
			Source     string `json:"source"`
			Confidence string `json:"confidence"`
			RunID      string `json:"run_id"`
			Query      string `json:"query"`
			Rank       int    `json:"rank"`
		} `json:"nudges"`
	}
	callTool(t, cs, "feedback_status", map[string]any{}, &inbox)
	if inbox.Pending != 1 || inbox.Nudges[0].Kind != "retrieval_outcome_missing" ||
		inbox.Nudges[0].RunID != search.RetrievalRunID || inbox.Nudges[0].Rank != 1 ||
		!strings.EqualFold(inbox.Nudges[0].Confidence, "high") {
		t.Fatalf("feedback inbox = %+v, want exact opened result", inbox)
	}

	var outcome struct{}
	callTool(t, cs, "record_retrieval_outcome", map[string]any{
		"run_id": search.RetrievalRunID, "rank": 1, "outcome": "helpful",
	}, &outcome)
	callTool(t, cs, "feedback_status", map[string]any{}, &inbox)
	if inbox.Pending != 0 {
		t.Fatalf("feedback pending = %d after label, want 0", inbox.Pending)
	}
}
