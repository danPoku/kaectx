// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
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
