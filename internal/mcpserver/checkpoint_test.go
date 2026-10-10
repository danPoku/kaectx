// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
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
