// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/search"
)

const defaultSearchLimit = 10

type searchContextArgs struct {
	Query     string `json:"query" jsonschema:"FTS5 keyword query (quote phrases, use AND/OR/NOT) to search across past coding-agent sessions in this project"`
	Agent     string `json:"agent,omitempty" jsonschema:"restrict to one agent's sessions, e.g. claude-code or codex (default: every agent)"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max results, default 10"`
	RiskAware bool   `json:"risk_aware,omitempty" jsonschema:"rerank using retrieval outcomes once evidence is credible (default false)"`
}

type searchContextHit struct {
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
}

type searchContextResult struct {
	Results []searchContextHit `json:"results"`
	// RetrievalRunID can be passed to future feedback/outcome tools without
	// exposing database rowids or relying on unstable chunk ids.
	RetrievalRunID    string `json:"retrieval_run_id,omitempty"`
	TelemetryRecorded bool   `json:"telemetry_recorded"`
	// UsedSemanticSearch tells the caller whether this ran keyword+semantic
	// fusion or fell back to keyword-only (no embedding worker has caught
	// up yet, or the local Ollama isn't reachable) — visible so an agent
	// doesn't mistake a degraded result set for the real thing.
	UsedSemanticSearch bool `json:"used_semantic_search"`
	GovernanceApplied  bool `json:"governance_applied"`
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
			"Returns short snippets, not full transcripts. To read a hit, call get_chunk with its chunk_id (the matching exchange, ~1k tokens), or get_session with its session_id and from_seq/to_seq set to the hit's first_seq/last_seq.",
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
	got, err := search.BestTrackedWithPolicy(ctx, s.db, s.embedClient, args.Query, s.projectID, args.Agent, limit, "mcp", args.RiskAware)
	if err != nil {
		return nil, searchContextResult{}, err
	}
	out := searchContextResult{Results: make([]searchContextHit, len(got.Results)), UsedSemanticSearch: got.UsedHybrid, RetrievalRunID: got.RunID, TelemetryRecorded: got.TelemetryErr == nil, GovernanceApplied: got.GovernanceApplied}
	for i, r := range got.Results {
		out.Results[i] = searchContextHit{
			ChunkID: r.ChunkID, SessionID: r.SessionID, Agent: r.Agent,
			StartedAt: r.StartedAt, At: r.At, Snippet: r.Snippet,
			FirstSeq: r.FirstSeq, LastSeq: r.LastSeq,
			Reliability: r.Reliability, PolicyScore: r.PolicyScore, GovernanceNeutral: r.GovernanceNeutral,
			OriginalRank: r.OriginalRank,
		}
	}
	return nil, out, nil
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
