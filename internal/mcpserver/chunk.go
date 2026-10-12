// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/store"
	"github.com/danPoku/kaectx/queries"
)

// getChunkMaxChars bounds one chunk's text. A chunk is normally ~900 tokens,
// but an unusually long assistant reply can exceed that.
const getChunkMaxChars = 12000

type getChunkArgs struct {
	ChunkID int64  `json:"chunk_id" jsonschema:"a chunk_id returned by search_context"`
	RunID   string `json:"run_id,omitempty" jsonschema:"run_id returned by the same search_context call; provide with rank to record an exact open"`
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
	// FeedbackTarget links the opened chunk to the search result it was opened
	// from: the one the caller named, or the inferred one when OpenInferred.
	// Opening is telemetry, not a helpful outcome.
	FeedbackTarget *feedbackTarget `json:"feedback_target,omitempty"`
	OpenRecorded   bool            `json:"open_recorded"`
	OpenInferred   bool            `json:"open_inferred,omitempty"`
	// NextStep is the exact labelling call for FeedbackTarget, so the agent
	// can copy it rather than reconstruct it from the tool descriptions.
	NextStep string `json:"next_step,omitempty"`
}

type feedbackTarget struct {
	RunID string `json:"run_id"`
	Rank  int    `json:"rank"`
}

func (s *server) registerChunk(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_chunk",
		Description: "Read one search hit in full: the user turn and the assistant's reply it belongs to (~1k tokens), by chunk_id from search_context. " +
			"Pass that result's run_id and rank together to record that exact result as opened; with chunk_id alone the open is attributed to the latest search that returned it, unless two recent searches did. " +
			"The result's next_step is the call that labels the hit once you know whether it helped. Use get_session only when you need the messages around it.",
	}, s.getChunk)
}

func (s *server) getChunk(_ context.Context, _ *mcp.CallToolRequest, args getChunkArgs) (*mcp.CallToolResult, getChunkResult, error) {
	// Half an exact target is a caller mistake, not a request to guess. Falling
	// through to inference would quietly swap the search the agent named for
	// one ctx picked.
	if (args.RunID != "") != (args.Rank > 0) {
		return nil, getChunkResult{}, fmt.Errorf("pass run_id and rank together, or neither")
	}
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
	// Open telemetry is fail-open, and exact when the caller passes run_id and
	// rank. A chunk ID alone cannot identify the originating search for certain
	// when agents run concurrently, so an open inferred from it is flagged.
	if args.RunID != "" && args.Rank > 0 {
		if s.recordOpen(args.RunID, args.Rank, args.ChunkID, false) {
			out.FeedbackTarget = &feedbackTarget{RunID: args.RunID, Rank: args.Rank}
			out.OpenRecorded = true
		}
	} else if runID, rank, ok := s.inferOpenTarget(args.ChunkID); ok && s.recordOpen(runID, rank, args.ChunkID, true) {
		// Most agents call get_chunk(chunk_id) and nothing else, which used to
		// leave no trace and so nothing to label. Attributing the read to the
		// search that most recently showed this chunk recovers those opens;
		// the inferred flag lets analysis drop them if the guess matters.
		out.FeedbackTarget = &feedbackTarget{RunID: runID, Rank: rank}
		out.OpenRecorded, out.OpenInferred = true, true
	}
	if out.FeedbackTarget != nil {
		// Asking in the result, at the moment of use, works better than the
		// server instructions an agent read once at the start of the session.
		out.NextStep = fmt.Sprintf("Once you know whether this helped, call record_retrieval_outcome(run_id=%q, rank=%d, outcome=helpful|unhelpful|stale|incorrect|superseded).",
			out.FeedbackTarget.RunID, out.FeedbackTarget.Rank)
	}
	return nil, out, nil
}

// inferOpenWindow bounds how far back inferOpenTarget looks for the search an
// untracked read came from. Agents read a hit within minutes of searching; a
// search from days ago that happened to return the same chunk is a different
// piece of work, and attributing the read to it would be a guess too far.
const inferOpenWindow = "-1 day"

// inferAmbiguityGap is how close together two searches that both returned the
// chunk must be for inferOpenTarget to refuse to choose between them. Within
// it, the read could as easily follow either search: two agents working at
// once, or one agent refining its query. A missed open costs one label; a
// wrong one pins a label to a search and rank the agent never saw.
const inferAmbiguityGap = 5 * time.Minute

// inferOpenTarget finds the most recent MCP search in this project, within
// inferOpenWindow, that returned chunkID, and that chunk's rank in it. It
// abstains when an earlier search also returned the chunk within
// inferAmbiguityGap, since retrieval_runs does not record which agent or
// session searched and so cannot tell the two apart.
//
// Only MCP searches count. `ctx search` prints no chunk IDs (cmd/ctx/search.go),
// so an agent that searched through the CLI has no chunk_id to pass to
// get_chunk, and a CLI search is more likely a person's. If the CLI ever
// prints chunk IDs, revisit this filter.
func (s *server) inferOpenTarget(chunkID int64) (runID string, rank int, ok bool) {
	rows, err := s.db.Query(`SELECT rc.run_id, rc.rank, rr.created_at
		FROM retrieval_candidates rc JOIN retrieval_runs rr ON rr.id=rc.run_id
		WHERE rc.chunk_id=? AND rr.project_id=? AND rr.surface='mcp'
		AND rr.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now',?)
		ORDER BY rr.created_at DESC, rr.rowid DESC, rc.rank ASC LIMIT 2`, chunkID, s.projectID, inferOpenWindow)
	if err != nil {
		return "", 0, false
	}
	defer rows.Close()
	type match struct {
		runID string
		rank  int
		at    time.Time
	}
	var found []match
	for rows.Next() {
		var m match
		var at string
		if err := rows.Scan(&m.runID, &m.rank, &at); err != nil {
			return "", 0, false
		}
		if m.at, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return "", 0, false
		}
		found = append(found, m)
	}
	if rows.Err() != nil || len(found) == 0 {
		return "", 0, false
	}
	if len(found) == 2 && found[0].at.Sub(found[1].at) < inferAmbiguityGap {
		return "", 0, false
	}
	return found[0].runID, found[0].rank, true
}

// recordOpen marks one candidate as opened and reports whether it did. The
// candidate's counters and last-open time move, and the open is appended to
// retrieval_open_events, in one transaction, so the two never disagree. It is
// fail-open: an observer failure must never hide context that was read
// successfully, so errors only mean the open goes unrecorded.
func (s *server) recordOpen(runID string, rank int, chunkID int64, inferred bool) bool {
	tx, err := s.db.Begin()
	if err != nil {
		return false
	}
	defer tx.Rollback()
	// opened_at has millisecond resolution, so two opens inside one
	// millisecond would share a time. Each open is therefore placed at least
	// 1ms after the result's previous open: times stay distinct and in order,
	// and are off by at most a millisecond per open in a burst.
	result, err := tx.Exec(`UPDATE retrieval_candidates
		SET opened_at=CASE
				WHEN opened_at IS NULL OR strftime('%Y-%m-%dT%H:%M:%fZ','now') > opened_at
				THEN strftime('%Y-%m-%dT%H:%M:%fZ','now')
				ELSE strftime('%Y-%m-%dT%H:%M:%fZ', opened_at, '+0.001 seconds')
			END,
			open_count=open_count+1,
			inferred_open_count=inferred_open_count+?
		WHERE run_id=? AND rank=? AND chunk_id=? AND EXISTS (
			SELECT 1 FROM retrieval_runs rr WHERE rr.id=retrieval_candidates.run_id AND rr.project_id=?
		)`, inferred, runID, rank, chunkID, s.projectID)
	if err != nil {
		return false
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return false
	}
	// The event takes the candidate's new opened_at so both name the same
	// instant.
	if _, err := tx.Exec(`INSERT INTO retrieval_open_events(run_id, rank, opened_at, inferred)
		SELECT run_id, rank, opened_at, ? FROM retrieval_candidates WHERE run_id=? AND rank=?`,
		inferred, runID, rank); err != nil {
		return false
	}
	return tx.Commit() == nil
}
