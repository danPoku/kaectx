// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/search"
)

const defaultSearchLimit = 10

type searchContextArgs struct {
	Query     string `json:"query" jsonschema:"FTS5 keyword query (quote phrases, use AND/OR/NOT) to search across past coding-agent sessions in this project"`
	Agent     string `json:"agent,omitempty" jsonschema:"restrict to one agent's sessions, e.g. claude-code or codex (default: every agent)"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max results, default 10"`
	RiskAware bool   `json:"risk_aware,omitempty" jsonschema:"rerank using retrieval outcomes once evidence is credible (default false)"`
	Since     string `json:"since,omitempty" jsonschema:"only turns at or after this time: a UTC date (2026-09-28) or RFC 3339 timestamp"`
	Until     string `json:"until,omitempty" jsonschema:"only turns before this time; a date means through the end of that day (2026-10-04)"`
}

type searchContextHit struct {
	Rank      int    `json:"rank"`
	ChunkID   int64  `json:"chunk_id"`
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	StartedAt string `json:"started_at"` // when the session began
	At        string `json:"at"`         // when this hit's turn happened
	Snippet   string `json:"snippet"`
	// FirstSeq..LastSeq is the message range of the matching exchange:
	// get_session(session_id, from_seq=first_seq, to_seq=last_seq) reads
	// just that, and get_chunk(chunk_id) returns its text directly.
	FirstSeq          int     `json:"first_seq"`
	LastSeq           int     `json:"last_seq"`
	Reliability       float64 `json:"reliability,omitempty"`
	PolicyScore       float64 `json:"policy_score,omitempty"`
	GovernanceNeutral bool    `json:"governance_neutral,omitempty"`
	OriginalRank      int     `json:"original_rank,omitempty"`
	// NextStep is the exact get_chunk call for this hit, run_id and rank
	// included. Agents copy a call from a result far more reliably than they
	// assemble one from a tool description, and without run_id and rank the
	// open could only be inferred.
	NextStep string `json:"next_step"`
}

type searchContextResult struct {
	Results []searchContextHit `json:"results"`
	// RunID is passed to get_chunk and record_retrieval_outcome, which name
	// it run_id too. It identifies the search without exposing database
	// rowids or relying on unstable chunk ids.
	RunID string `json:"run_id,omitempty"`
	// RetrievalRunID is the old name for RunID, kept for one release so
	// clients that read it keep working. Deprecated: use run_id.
	RetrievalRunID    string `json:"retrieval_run_id,omitempty"`
	TelemetryRecorded bool   `json:"telemetry_recorded"`
	// UsedSemanticSearch tells the caller whether this ran keyword+semantic
	// fusion or fell back to keyword-only (no embedding worker has caught
	// up yet, or the local Ollama isn't reachable) — visible so an agent
	// doesn't mistake a degraded result set for the real thing.
	UsedSemanticSearch bool `json:"used_semantic_search"`
	GovernanceApplied  bool `json:"governance_applied"`
	// Index says how current the searched index is; read its warning
	// before concluding that something never happened.
	Index *indexStatus `json:"index,omitempty"`
}

type searchNotesArgs struct {
	Query string `json:"query" jsonschema:"FTS5 keyword query to search saved notes"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results, default 10"`
}

type noteHit struct {
	ID        int64    `json:"id"`
	Kind      string   `json:"kind"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Tags      []string `json:"tags,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	CreatedAt string   `json:"created_at"`
}

type searchNotesResult struct {
	Notes []noteHit `json:"notes"`
}

func (s *server) registerSearch(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "search_context",
		Description: "Keyword search over past coding-agent sessions in this project — across every agent that's been ingested (Claude Code, Codex, ...), not just the one calling this tool. " +
			"Returns short snippets, not full transcripts. To read a hit, make the call in its next_step: get_chunk with its chunk_id, the result's run_id and the hit's rank (the matching exchange, ~1k tokens). Or call get_session with its session_id and from_seq/to_seq set to the hit's first_seq/last_seq. " +
			"For questions about a period (\"what happened last week\"), set since/until: otherwise older, stronger matches outrank the period you asked about. " +
			"If the result's index.warning is set, ctx has not read some recent logs yet; say so rather than concluding nothing happened.",
	}, s.searchContext)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_notes",
		Description: "Search notes — decisions, gotchas, conventions, and todos deliberately recorded with save_note. Distinct from search_context: notes are curated policy an agent chose to write down, not raw transcript.",
	}, s.searchNotes)
}

func (s *server) searchContext(ctx context.Context, _ *mcp.CallToolRequest, args searchContextArgs) (*mcp.CallToolResult, searchContextResult, error) {
	limit := args.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	w, err := search.ParseWindow(args.Since, args.Until)
	if err != nil {
		return nil, searchContextResult{}, err
	}
	got, err := search.BestTrackedIn(ctx, s.db, s.embedClient, args.Query, s.projectID, args.Agent, w, limit, "mcp", args.RiskAware)
	if err != nil {
		return nil, searchContextResult{}, err
	}
	out := searchContextResult{Results: make([]searchContextHit, len(got.Results)), UsedSemanticSearch: got.UsedHybrid, RunID: got.RunID, RetrievalRunID: got.RunID, TelemetryRecorded: got.TelemetryErr == nil, GovernanceApplied: got.GovernanceApplied, Index: s.indexStatus()}
	for i, r := range got.Results {
		out.Results[i] = searchContextHit{
			Rank: i + 1, ChunkID: r.ChunkID, SessionID: r.SessionID, Agent: r.Agent,
			StartedAt: r.StartedAt, At: r.At, Snippet: r.Snippet,
			FirstSeq: r.FirstSeq, LastSeq: r.LastSeq,
			Reliability: r.Reliability, PolicyScore: r.PolicyScore, GovernanceNeutral: r.GovernanceNeutral,
			OriginalRank: r.OriginalRank,
			NextStep:     getChunkCall(r.ChunkID, got.RunID, i+1),
		}
	}
	return nil, out, nil
}

// getChunkCall writes the get_chunk call that opens one hit. With no run ID
// (telemetry failed to record the search) there is nothing to attribute the
// open to, so the call is the plain read.
func getChunkCall(chunkID int64, runID string, rank int) string {
	if runID == "" {
		return fmt.Sprintf("get_chunk(chunk_id=%d)", chunkID)
	}
	return fmt.Sprintf("get_chunk(chunk_id=%d, run_id=%q, rank=%d)", chunkID, runID, rank)
}

func (s *server) searchNotes(_ context.Context, _ *mcp.CallToolRequest, args searchNotesArgs) (*mcp.CallToolResult, searchNotesResult, error) {
	limit := args.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	rows, err := search.Notes(s.db, args.Query, s.projectID, limit)
	if err != nil {
		return nil, searchNotesResult{}, err
	}
	out := searchNotesResult{Notes: make([]noteHit, len(rows))}
	for i, r := range rows {
		out.Notes[i] = noteHit{
			ID: r.ID, Kind: r.Kind, Title: r.Title, Body: r.Body,
			Tags: r.Tags, Agent: r.Agent, CreatedAt: r.CreatedAt,
		}
	}
	return nil, out, nil
}
