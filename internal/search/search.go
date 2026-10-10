// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package search runs the retrieval queries defined in queries/retrieval.sql
// against the store. Milestone 2 only wires up keyword (FTS) search;
// SearchHybrid needs the embedding worker (milestone 5).
package search

import (
	"database/sql"
	"encoding/json"

	"github.com/danPoku/kaectx/internal/store"
	"github.com/danPoku/kaectx/queries"
)

// KeywordResult is one row of SearchKeyword's result set.
type KeywordResult struct {
	ChunkID   int64
	SessionID string
	Agent     string
	StartedAt string
	Snippet   string
	BM25      float64
	// FirstSeq and LastSeq are the message range this chunk covers, so a
	// caller can fetch exactly the matching exchange from get_session.
	FirstSeq int
	LastSeq  int
	// At is when this chunk's turn happened (its first message's timestamp),
	// falling back to the session start. StartedAt is the SESSION's start,
	// which is the same for every chunk in a long session.
	At string
}

// Keyword runs SearchKeyword: BM25-ranked full-text search over chunks,
// scoped to a project and optionally to one agent.
func Keyword(db *sql.DB, query string, projectID int64, agent string, limit int) ([]KeywordResult, error) {
	return KeywordIn(db, query, projectID, agent, Window{}, limit)
}

// KeywordIn is Keyword limited to turns inside w.
func KeywordIn(db *sql.DB, query string, projectID int64, agent string, w Window, limit int) ([]KeywordResult, error) {
	sqlText, err := store.LoadQuery(queries.FS, "retrieval.sql", "SearchKeyword")
	if err != nil {
		return nil, err
	}

	var agentArg any
	if agent != "" {
		agentArg = agent
	}

	rows, err := db.Query(sqlText, append([]any{
		sql.Named("query", sanitizeFTS5Query(query)),
		sql.Named("project_id", projectID),
		sql.Named("agent", agentArg),
		sql.Named("limit", limit),
	}, w.Args()...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []KeywordResult
	for rows.Next() {
		var r KeywordResult
		if err := rows.Scan(&r.ChunkID, &r.SessionID, &r.Agent, &r.StartedAt, &r.Snippet, &r.BM25, &r.FirstSeq, &r.LastSeq, &r.At); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// NoteResult is one row of SearchNotes's result set.
type NoteResult struct {
	ID        int64
	Kind      string
	Title     string
	Body      string
	Tags      []string
	Agent     string
	CreatedAt string
}

// Notes runs SearchNotes: keyword search over live (non-superseded) notes,
// scoped to a project plus any global notes (project_id IS NULL).
func Notes(db *sql.DB, query string, projectID int64, limit int) ([]NoteResult, error) {
	sqlText, err := store.LoadQuery(queries.FS, "retrieval.sql", "SearchNotes")
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(sqlText,
		sql.Named("query", sanitizeFTS5Query(query)),
		sql.Named("project_id", projectID),
		sql.Named("limit", limit),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []NoteResult
	for rows.Next() {
		var r NoteResult
		var tagsJSON string
		var agent sql.NullString
		if err := rows.Scan(&r.ID, &r.Kind, &r.Title, &r.Body, &tagsJSON, &agent, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Agent = agent.String
		// tags is CHECKed as a JSON array in the schema, so this can only
		// fail if the column itself is somehow malformed — degrade to an
		// empty list rather than fail the whole search over one bad row.
		_ = json.Unmarshal([]byte(tagsJSON), &r.Tags)
		results = append(results, r)
	}
	return results, rows.Err()
}
