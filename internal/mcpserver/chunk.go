// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/store"
	"github.com/danPoku/kaectx/queries"
)

// getChunkMaxChars bounds one chunk's text. A chunk is normally ~900 tokens,
// but an unusually long assistant reply can exceed that.
const getChunkMaxChars = 12000

type getChunkArgs struct {
	ChunkID int64  `json:"chunk_id" jsonschema:"a chunk_id returned by search_context"`
	RunID   string `json:"run_id,omitempty" jsonschema:"retrieval_run_id returned by the same search_context call; provide with rank to record an exact open"`
	Rank    int    `json:"rank,omitempty" jsonschema:"1-based rank returned with this chunk by search_context; provide with run_id to record an exact open"`
}

type getChunkResult struct {
	ChunkID   int64  `json:"chunk_id"`
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	At        string `json:"at"`
	FirstSeq  int    `json:"first_seq"`
	LastSeq   int    `json:"last_seq"`
	// Text is the exchange: "User: ..." then "Assistant: ...", conversation
	// text only, exactly what search indexed.
	Text          string `json:"text"`
	TextTruncated bool   `json:"text_truncated,omitempty"`
	// FeedbackTarget links the opened chunk to the exact search target supplied
	// by the caller. Opening is telemetry, not a helpful outcome.
	FeedbackTarget *feedbackTarget `json:"feedback_target,omitempty"`
	OpenRecorded   bool            `json:"open_recorded"`
}

type feedbackTarget struct {
	RunID string `json:"run_id"`
	Rank  int    `json:"rank"`
}

func (s *server) registerChunk(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_chunk",
		Description: "Read one search hit in full: the user turn and the assistant's reply it belongs to (~1k tokens), by chunk_id from search_context. " +
			"Pass that result's run_id and rank to record that exact result as opened; omit them for an untracked direct read. Use get_session only when you need the messages around it.",
	}, s.getChunk)
}

func (s *server) getChunk(_ context.Context, _ *mcp.CallToolRequest, args getChunkArgs) (*mcp.CallToolResult, getChunkResult, error) {
	sqlText, err := store.LoadQuery(queries.FS, "retrieval.sql", "GetChunk")
	if err != nil {
		return nil, getChunkResult{}, err
	}
	var out getChunkResult
	err = s.db.QueryRow(sqlText, sql.Named("chunk_id", args.ChunkID), sql.Named("project_id", s.projectID)).
		Scan(&out.ChunkID, &out.SessionID, &out.Agent, &out.FirstSeq, &out.LastSeq, &out.Text, &out.At)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, getChunkResult{}, fmt.Errorf("no chunk %d in this project", args.ChunkID)
	}
	if err != nil {
		return nil, getChunkResult{}, err
	}
	if cut, _, ok := truncateChars(out.Text, getChunkMaxChars); ok {
		out.Text, out.TextTruncated = cut, true
	}
	// Open telemetry is exact and fail-open. A chunk ID alone is not enough to
	// identify the originating search when agents run concurrently, and an
	// observer failure must never hide context that was read successfully.
	if args.RunID != "" && args.Rank > 0 {
		result, updateErr := s.db.Exec(`UPDATE retrieval_candidates
			SET opened_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'), open_count=open_count+1
			WHERE run_id=? AND rank=? AND chunk_id=? AND EXISTS (
				SELECT 1 FROM retrieval_runs rr WHERE rr.id=retrieval_candidates.run_id AND rr.project_id=?
			)`, args.RunID, args.Rank, args.ChunkID, s.projectID)
		if updateErr == nil {
			if affected, rowsErr := result.RowsAffected(); rowsErr == nil && affected == 1 {
				out.FeedbackTarget = &feedbackTarget{RunID: args.RunID, Rank: args.Rank}
				out.OpenRecorded = true
			}
		}
	}
	return nil, out, nil
}
