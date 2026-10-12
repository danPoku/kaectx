// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/governance"
	"github.com/danPoku/kaectx/internal/redact"
)

type recordOutcomeArgs struct {
	RunID   string `json:"run_id" jsonschema:"run_id returned by search_context or get_chunk"`
	Rank    int    `json:"rank" jsonschema:"1-based result rank returned by search_context or get_chunk"`
	Outcome string `json:"outcome" jsonschema:"one of helpful, unhelpful, stale, incorrect, superseded"`
	Note    string `json:"note,omitempty" jsonschema:"optional explanation; secrets are redacted before storage"`
}

type recordOutcomeResult struct {
	Recorded     bool    `json:"recorded"`
	EvidenceKey  string  `json:"evidence_key"`
	Observations int     `json:"observations"`
	Reliability  float64 `json:"reliability"`
	Neutral      bool    `json:"neutral"`
}

type feedbackStatusArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum number of nudges to return; defaults to 10 and is capped at 25"`
}

type feedbackNudge struct {
	Kind       string `json:"kind"`
	Source     string `json:"source"`
	Confidence string `json:"confidence"`
	Prompt     string `json:"prompt"`
	SessionID  string `json:"session_id,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	Rank       int    `json:"rank,omitempty"`
	Query      string `json:"query,omitempty"`
}

type feedbackStatusResult struct {
	Pending int             `json:"pending"`
	Nudges  []feedbackNudge `json:"nudges"`
}

type checkpointOutcomeArgs struct {
	SessionID string `json:"session_id" jsonschema:"session_id returned by checkpoint_status or feedback_status"`
	Outcome   string `json:"outcome" jsonschema:"one of started_new_session, saved_handoff, continued, dismissed"`
	Note      string `json:"note,omitempty" jsonschema:"optional explanation; secrets are redacted before storage"`
}

type checkpointOutcomeResult struct {
	Recorded  bool   `json:"recorded"`
	SessionID string `json:"session_id"`
	Outcome   string `json:"outcome"`
}

var validOutcomes = map[string]bool{
	"helpful": true, "unhelpful": true, "stale": true,
	"incorrect": true, "superseded": true,
}

var validCheckpointOutcomes = map[string]bool{
	"started_new_session": true, "saved_handoff": true,
	"continued": true, "dismissed": true,
}

func (s *server) registerFeedback(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "record_retrieval_outcome",
		Description: "Record whether retrieved context was helpful, unhelpful, stale, incorrect, or superseded once its usefulness is known. Use the exact run_id and rank returned by search_context or get_chunk. Record the honest result even when negative. Opening is tracked separately and never counts as helpful.",
	}, s.recordRetrievalOutcome)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "feedback_status",
		Description: "Return a small inbox of high-confidence feedback opportunities derived from recorded CTX activity. Call at natural milestones. A nudge reports missing feedback, never infers that retrieved context was helpful or harmful. Ask only when the answer is already apparent or the interruption is worthwhile.",
	}, s.feedbackStatus)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "record_checkpoint_outcome",
		Description: "Record the user's response to a checkpoint recommendation. Use the exact session_id and one of started_new_session, saved_handoff, continued, or dismissed.",
	}, s.recordCheckpointOutcome)
}

func (s *server) recordRetrievalOutcome(_ context.Context, _ *mcp.CallToolRequest, args recordOutcomeArgs) (*mcp.CallToolResult, recordOutcomeResult, error) {
	if !validOutcomes[args.Outcome] {
		return nil, recordOutcomeResult{}, fmt.Errorf("a valid outcome is required")
	}
	if args.RunID == "" || args.Rank <= 0 {
		return nil, recordOutcomeResult{}, fmt.Errorf("run_id and a positive rank are required")
	}
	// The candidate's range is the exchange as the agent saw it. Its first
	// seq anchors the label, so the label keeps counting if the chunk is
	// later rebuilt with different bounds.
	var e governance.Evidence
	err := s.db.QueryRow(`SELECT rc.evidence_key, rc.session_id, rc.first_seq, rc.last_seq
		FROM retrieval_candidates rc JOIN retrieval_runs rr ON rr.id=rc.run_id
		WHERE rc.run_id=? AND rc.rank=? AND rr.project_id=?`, args.RunID, args.Rank, s.projectID).Scan(&e.Key, &e.SessionID, &e.FirstSeq, &e.LastSeq)
	if err == sql.ErrNoRows {
		return nil, recordOutcomeResult{}, fmt.Errorf("retrieval result not found in this project")
	}
	if err != nil {
		return nil, recordOutcomeResult{}, err
	}
	var note any
	if args.Note != "" {
		note = redact.Redact(args.Note)
	}
	if _, err := s.db.Exec(`INSERT INTO memory_observations(project_id,evidence_key,session_id,anchor_seq,run_id,candidate_rank,outcome,note)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id,candidate_rank) WHERE run_id IS NOT NULL DO UPDATE SET
			outcome=excluded.outcome, note=excluded.note,
			created_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')`, s.projectID, e.Key, e.SessionID, e.FirstSeq, args.RunID, args.Rank, args.Outcome, note); err != nil {
		return nil, recordOutcomeResult{}, err
	}
	a, err := governance.Assess(s.db, s.projectID, e)
	if err != nil {
		return nil, recordOutcomeResult{}, err
	}
	return nil, recordOutcomeResult{Recorded: true, EvidenceKey: e.Key, Observations: a.Observations, Reliability: a.Reliability, Neutral: a.Neutral}, nil
}

func (s *server) feedbackStatus(_ context.Context, _ *mcp.CallToolRequest, args feedbackStatusArgs) (*mcp.CallToolResult, feedbackStatusResult, error) {
	limit := args.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 25 {
		limit = 25
	}
	out := feedbackStatusResult{Nudges: []feedbackNudge{}}

	rows, err := s.db.Query(`SELECT session_id, level FROM checkpoint_observations
		WHERE project_id=? AND outcome IS NULL ORDER BY recommended_at DESC LIMIT ?`, s.projectID, limit)
	if err != nil {
		return nil, out, err
	}
	for rows.Next() {
		var sessionID, level string
		if err := rows.Scan(&sessionID, &level); err != nil {
			rows.Close()
			return nil, out, err
		}
		out.Nudges = append(out.Nudges, feedbackNudge{
			Kind: "checkpoint_outcome_missing", Source: "checkpoint_observation", Confidence: "high",
			Prompt:    "Record whether the user started a new session, saved a handoff, continued, or dismissed the recommendation.",
			SessionID: sessionID,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, out, err
	}
	if err := rows.Close(); err != nil {
		return nil, out, err
	}
	remaining := limit - len(out.Nudges)
	if remaining > 0 {
		// A result opened only by inferred reads may be credited to the wrong
		// search, so its nudge says so instead of claiming high confidence.
		rows, err = s.db.Query(`SELECT rc.run_id, rc.rank, rr.query,
				rc.inferred_open_count>0 AND rc.inferred_open_count=rc.open_count
			FROM retrieval_candidates rc JOIN retrieval_runs rr ON rr.id=rc.run_id
			WHERE rr.project_id=? AND rc.opened_at IS NOT NULL
			AND NOT EXISTS (SELECT 1 FROM memory_observations mo
				WHERE mo.run_id=rc.run_id AND mo.candidate_rank=rc.rank)
			ORDER BY rc.opened_at DESC LIMIT ?`, s.projectID, remaining)
		if err != nil {
			return nil, out, err
		}
		defer rows.Close()
		for rows.Next() {
			var runID, query string
			var rank int
			var inferredOnly bool
			if err := rows.Scan(&runID, &rank, &query, &inferredOnly); err != nil {
				return nil, out, err
			}
			nudge := feedbackNudge{
				Kind: "retrieval_outcome_missing", Source: "opened_result", Confidence: "high",
				Prompt: "If this retrieved evidence influenced the work, record its honest outcome; opening it alone is not a helpful label.",
				RunID:  runID, Rank: rank, Query: query,
			}
			if inferredOnly {
				nudge.Source, nudge.Confidence = "inferred_open", "medium"
				nudge.Prompt = "This open was matched to a search from the chunk alone. If you ran this query and the evidence influenced the work, record its honest outcome; otherwise skip it."
			}
			out.Nudges = append(out.Nudges, nudge)
		}
		if err := rows.Err(); err != nil {
			return nil, out, err
		}
	}
	out.Pending = len(out.Nudges)
	return nil, out, nil
}

func (s *server) recordCheckpointOutcome(_ context.Context, _ *mcp.CallToolRequest, args checkpointOutcomeArgs) (*mcp.CallToolResult, checkpointOutcomeResult, error) {
	if args.SessionID == "" || !validCheckpointOutcomes[args.Outcome] {
		return nil, checkpointOutcomeResult{}, fmt.Errorf("session_id and a valid checkpoint outcome are required")
	}
	var note any
	if args.Note != "" {
		note = redact.Redact(args.Note)
	}
	result, err := s.db.Exec(`UPDATE checkpoint_observations
		SET outcome=?, note=?, resolved_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE session_id=? AND project_id=?`, args.Outcome, note, args.SessionID, s.projectID)
	if err != nil {
		return nil, checkpointOutcomeResult{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, checkpointOutcomeResult{}, err
	}
	if n == 0 {
		return nil, checkpointOutcomeResult{}, fmt.Errorf("checkpoint recommendation not found in this project")
	}
	return nil, checkpointOutcomeResult{Recorded: true, SessionID: args.SessionID, Outcome: args.Outcome}, nil
}
