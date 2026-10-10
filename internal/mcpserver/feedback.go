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
	RunID   string `json:"run_id" jsonschema:"retrieval_run_id returned by search_context"`
	Rank    int    `json:"rank" jsonschema:"1-based rank of the result being assessed"`
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

var validOutcomes = map[string]bool{
	"helpful": true, "unhelpful": true, "stale": true,
	"incorrect": true, "superseded": true,
}

func (s *server) registerFeedback(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "record_retrieval_outcome",
		Description: "Record whether a search_context result was helpful, unhelpful, stale, incorrect, or superseded. Use the retrieval_run_id and 1-based result rank returned by search_context. This evidence is measured first and does not yet alter ranking.",
	}, s.recordRetrievalOutcome)
}

func (s *server) recordRetrievalOutcome(_ context.Context, _ *mcp.CallToolRequest, args recordOutcomeArgs) (*mcp.CallToolResult, recordOutcomeResult, error) {
	if args.RunID == "" || args.Rank <= 0 || !validOutcomes[args.Outcome] {
		return nil, recordOutcomeResult{}, fmt.Errorf("run_id, positive rank, and a valid outcome are required")
	}
	var evidenceKey string
	err := s.db.QueryRow(`SELECT rc.evidence_key
		FROM retrieval_candidates rc JOIN retrieval_runs rr ON rr.id=rc.run_id
		WHERE rc.run_id=? AND rc.rank=? AND rr.project_id=?`, args.RunID, args.Rank, s.projectID).Scan(&evidenceKey)
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
	if _, err := s.db.Exec(`INSERT INTO memory_observations(project_id,evidence_key,run_id,candidate_rank,outcome,note)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(run_id,candidate_rank) WHERE run_id IS NOT NULL DO UPDATE SET
			outcome=excluded.outcome, note=excluded.note,
			created_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')`, s.projectID, evidenceKey, args.RunID, args.Rank, args.Outcome, note); err != nil {
		return nil, recordOutcomeResult{}, err
	}
	a, err := governance.Assess(s.db, s.projectID, evidenceKey)
	if err != nil {
		return nil, recordOutcomeResult{}, err
	}
	return nil, recordOutcomeResult{Recorded: true, EvidenceKey: evidenceKey, Observations: a.Observations, Reliability: a.Reliability, Neutral: a.Neutral}, nil
}
