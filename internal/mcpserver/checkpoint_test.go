// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
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

// timeMessages gives the fixture session exactly these message times, in
// order: the fixture's own timestamps are cleared and one bare event message
// is added per time, so a test states the session's rhythm outright.
func timeMessages(t *testing.T, db *sql.DB, times []time.Time) {
	t.Helper()
	var sessionID string
	if err := db.QueryRow(`SELECT id FROM sessions LIMIT 1`).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE messages SET created_at = NULL WHERE session_id = ?`, sessionID); err != nil {
		t.Fatal(err)
	}
	for i, at := range times {
		if _, err := db.Exec(`INSERT INTO messages(session_id, seq, role, kind, content, created_at)
			VALUES (?, ?, 'system', 'event', '', ?)`, sessionID, 100000+i, at.UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
}

// spaced returns n times, each gap after the one before.
func spaced(start time.Time, gap time.Duration, n int) []time.Time {
	out := make([]time.Time, n)
	for i := range out {
		out[i] = start.Add(time.Duration(i) * gap)
	}
	return out
}

// bursts returns count bursts of three messages two minutes apart, the bursts
// starting every period: a session someone keeps coming back to.
func bursts(start time.Time, period time.Duration, count int) []time.Time {
	var out []time.Time
	for i := 0; i < count; i++ {
		out = append(out, spaced(start.Add(time.Duration(i)*period), 2*time.Minute, 3)...)
	}
	return out
}

// TestCheckpointStatusLevels pins the level rules of #41: time means active
// time, and only an urgent threshold makes a recommendation strong.
func TestCheckpointStatusLevels(t *testing.T) {
	start := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		messages    int
		times       []time.Time
		wantLevel   string
		wantSignal  bool
		wantReasons int
		wantMinutes int
	}{
		{
			name:     "one hour of steady work is healthy",
			messages: 120, times: spaced(start, 20*time.Minute, 4),
			wantLevel: "healthy", wantMinutes: 60,
		},
		{
			// The session #41 was filed for: 33 messages, resumed over 25
			// days. 11 bursts of 4 minutes each is 44 active minutes; the
			// old span measure called this 36,000.
			name:     "33 messages spread over 25 days with short gaps is healthy",
			messages: 33, times: bursts(start, 60*time.Hour, 11),
			wantLevel: "healthy", wantMinutes: 44,
		},
		{
			name:     "a gap of exactly 30 minutes still counts, 31 does not",
			messages: 10, times: []time.Time{start, start.Add(30 * time.Minute), start.Add(61 * time.Minute)},
			wantLevel: "healthy", wantMinutes: 30,
		},
		{
			name:     "over the message threshold only",
			messages: 400, times: spaced(start, 20*time.Minute, 4),
			wantLevel: "checkpoint_recommended", wantSignal: true, wantReasons: 1, wantMinutes: 60,
		},
		{
			name:     "over the active-time threshold only",
			messages: 50, times: spaced(start, 20*time.Minute, 13),
			wantLevel: "checkpoint_recommended", wantSignal: true, wantReasons: 1, wantMinutes: 240,
		},
		{
			// Two reasons used to be enough to escalate, which is why the
			// lower level never showed in practice.
			name:     "two thresholds, neither urgent, is still only recommended",
			messages: 500, times: spaced(start, 20*time.Minute, 16),
			wantLevel: "checkpoint_recommended", wantSignal: true, wantReasons: 2, wantMinutes: 300,
		},
		{
			name:     "urgent message count is strong on its own",
			messages: 800, times: spaced(start, 20*time.Minute, 4),
			wantLevel: "checkpoint_strongly_recommended", wantSignal: true, wantReasons: 1, wantMinutes: 60,
		},
		{
			name:     "urgent active time is strong on its own",
			messages: 50, times: spaced(start, 20*time.Minute, 25),
			wantLevel: "checkpoint_strongly_recommended", wantSignal: true, wantReasons: 1, wantMinutes: 480,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs, db := testServer(t)
			timeMessages(t, db, c.times)
			// message_count is set after the messages go in, because a trigger
			// bumps it on every insert. started_at and ended_at are set a month
			// apart on purpose: the span must no longer influence anything.
			if _, err := db.Exec(`UPDATE sessions SET message_count = ?,
				started_at = '2026-09-01T00:00:00Z', ended_at = '2026-10-01T00:00:00Z'`, c.messages); err != nil {
				t.Fatal(err)
			}

			var out checkpointOut
			callTool(t, cs, "checkpoint_status", map[string]any{}, &out)
			if out.Level != c.wantLevel || out.ShouldSignal != c.wantSignal {
				t.Fatalf("level = %q, should_signal = %v; want %q, %v (reasons %v)", out.Level, out.ShouldSignal, c.wantLevel, c.wantSignal, out.Reasons)
			}
			if len(out.Reasons) != c.wantReasons {
				t.Errorf("reasons = %v, want %d", out.Reasons, c.wantReasons)
			}
			if out.Metrics.Messages != c.messages || out.Metrics.DurationMins != c.wantMinutes {
				t.Errorf("metrics = %+v, want %d messages and %d active minutes", out.Metrics, c.messages, c.wantMinutes)
			}
			for _, r := range out.Reasons {
				if strings.Contains(r, "span") {
					t.Errorf("reason %q still speaks of a span; it must say active time", r)
				}
			}
		})
	}
}

func checkpointRow(t *testing.T, db *sql.DB, sessionID string) (level string, outcome sql.NullString) {
	t.Helper()
	if err := db.QueryRow(`SELECT level, outcome FROM checkpoint_observations WHERE session_id = ?`, sessionID).Scan(&level, &outcome); err != nil {
		t.Fatalf("read checkpoint row for %s: %v", sessionID, err)
	}
	return level, outcome
}

// #41: a healthy check is stored, "continued" can be recorded against it, and
// nothing else can. Without the row there was no comparison group.
func TestHealthyCheckIsStoredAndTakesOnlyContinued(t *testing.T) {
	cs, db := testServer(t)
	if _, err := db.Exec(`UPDATE sessions SET message_count = 120`); err != nil {
		t.Fatal(err)
	}
	var check checkpointOut
	callTool(t, cs, "checkpoint_status", map[string]any{}, &check)
	if check.Level != "healthy" {
		t.Fatalf("setup: level = %q, want healthy", check.Level)
	}
	if level, outcome := checkpointRow(t, db, check.SessionID); level != "healthy" || outcome.Valid {
		t.Fatalf("stored row = %q / %+v, want healthy with no outcome", level, outcome)
	}

	// A healthy row asks nothing, so the inbox must not nudge about it.
	var inbox struct {
		Nudges []struct {
			Kind string `json:"kind"`
		} `json:"nudges"`
	}
	callTool(t, cs, "feedback_status", map[string]any{}, &inbox)
	for _, n := range inbox.Nudges {
		if n.Kind == "checkpoint_outcome_missing" {
			t.Errorf("feedback_status nudged about a healthy check: %+v", inbox.Nudges)
		}
	}

	for _, outcome := range []string{"saved_handoff", "started_new_session", "dismissed"} {
		res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{
			Name:      "record_checkpoint_outcome",
			Arguments: map[string]any{"session_id": check.SessionID, "outcome": outcome},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Errorf("record_checkpoint_outcome(%s) on a healthy check succeeded, want an error", outcome)
		}
	}
	if _, outcome := checkpointRow(t, db, check.SessionID); outcome.Valid {
		t.Fatalf("a rejected outcome was stored: %q", outcome.String)
	}

	var recorded struct {
		Recorded bool `json:"recorded"`
	}
	callTool(t, cs, "record_checkpoint_outcome", map[string]any{"session_id": check.SessionID, "outcome": "continued"}, &recorded)
	if level, outcome := checkpointRow(t, db, check.SessionID); !recorded.Recorded || level != "healthy" || outcome.String != "continued" {
		t.Fatalf("after continued: recorded=%v row = %q / %+v", recorded.Recorded, level, outcome)
	}

	// Checking again while still healthy keeps the answer, and keeps one row.
	callTool(t, cs, "checkpoint_status", map[string]any{}, &check)
	if _, outcome := checkpointRow(t, db, check.SessionID); outcome.String != "continued" {
		t.Errorf("a repeat healthy check dropped the outcome: %+v", outcome)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM checkpoint_observations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("checkpoint_observations has %d rows, want one per session", rows)
	}

	// Once the session earns a recommendation, that is a new question: the
	// old "continued" must not stand in as its answer.
	if _, err := db.Exec(`UPDATE sessions SET message_count = 400`); err != nil {
		t.Fatal(err)
	}
	callTool(t, cs, "checkpoint_status", map[string]any{}, &check)
	if level, outcome := checkpointRow(t, db, check.SessionID); level != "checkpoint_recommended" || outcome.Valid {
		t.Errorf("after crossing a threshold: row = %q / %+v, want checkpoint_recommended with the outcome cleared", level, outcome)
	}
}

// With no check on record there is nothing to answer, healthy or otherwise.
func TestRecordCheckpointOutcomeNeedsACheck(t *testing.T) {
	cs, db := testServer(t)
	var sessionID string
	if err := db.QueryRow(`SELECT id FROM sessions LIMIT 1`).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "record_checkpoint_outcome",
		Arguments: map[string]any{"session_id": sessionID, "outcome": "continued"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("record_checkpoint_outcome with no check on record succeeded, want an error")
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
