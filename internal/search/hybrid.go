// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/danPoku/kaectx/internal/embed"
	"github.com/danPoku/kaectx/internal/store"
	"github.com/danPoku/kaectx/queries"
)

// HybridResult is one row of SearchHybrid/SearchHybridByAgent's result set.
type HybridResult struct {
	ChunkID   int64
	Score     float64
	SessionID string
	Agent     string
	Title     string
	StartedAt string
	Preview   string // hit-centred excerpt of the chunk; see centerSnippet
	FirstSeq  int    // the chunk's message range, as in KeywordResult
	LastSeq   int
	At        string // when this chunk's turn happened; see KeywordResult.At
}

// MaxSemanticDistance is the farthest cosine distance (0 = identical, 2 =
// opposite) at which a vector neighbour still counts as a semantic match.
//
// Calibrated on real ctx data with nomic-embed-text: exact and near-exact
// matches sit at 0.14 to 0.30, genuinely on-topic paraphrases at 0.33 to
// 0.47, and the BEST match for an off-topic query (a recipe, a revenue
// forecast) at 0.56 to 0.60. 0.50 splits the last two groups. Recalibrate if
// the embedding model changes; the corpus it was measured on was small.
const MaxSemanticDistance = 0.50

// Nearest-neighbour candidates fetched for the semantic half. vec0 can't
// filter by time inside the KNN search, so under a date window more
// candidates are fetched and the window applied after; otherwise the 50
// nearest chunks across the whole project could all be out of range,
// leaving a windowed search keyword-only without saying so.
const (
	semanticK         = 50
	semanticKWindowed = 400
)

// Hybrid runs Reciprocal Rank Fusion of keyword (FTS) and semantic (vector)
// search over chunks. It embeds query itself via client, so callers don't
// need to know embedding details — same contract as Keyword, just backed by
// two ranking signals instead of one.
//
// SearchHybrid and SearchHybridByAgent are two separate named queries (see
// retrieval.sql's comment on SearchHybridByAgent for why vec0 needs its own
// copy rather than a shared ":agent IS NULL OR ..." clause); which one runs
// is an implementation detail callers shouldn't need to think about.
func Hybrid(ctx context.Context, db *sql.DB, client embed.Embedder, query string, projectID int64, agent string, limit int) ([]HybridResult, error) {
	return HybridIn(ctx, db, client, query, projectID, agent, Window{}, limit)
}

// HybridIn is Hybrid limited to turns inside w.
func HybridIn(ctx context.Context, db *sql.DB, client embed.Embedder, query string, projectID int64, agent string, w Window, limit int) ([]HybridResult, error) {
	vec, err := client.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	vecJSON, err := json.Marshal(vec)
	if err != nil {
		return nil, err
	}

	semK := semanticK
	if !w.IsZero() {
		semK = semanticKWindowed
	}
	queryName := "SearchHybrid"
	args := []any{
		sql.Named("query", sanitizeFTS5Query(query)),
		sql.Named("query_embedding", string(vecJSON)),
		sql.Named("project_id", projectID),
		sql.Named("max_distance", MaxSemanticDistance),
		sql.Named("limit", limit),
		sql.Named("sem_k", semK),
	}
	args = append(args, w.Args()...)
	if agent != "" {
		queryName = "SearchHybridByAgent"
		args = append(args, sql.Named("agent", agent))
	}

	sqlText, err := store.LoadQuery(queries.FS, "retrieval.sql", queryName)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []HybridResult
	for rows.Next() {
		var r HybridResult
		var title sql.NullString
		if err := rows.Scan(&r.ChunkID, &r.Score, &r.SessionID, &r.Agent, &title, &r.StartedAt, &r.Preview, &r.FirstSeq, &r.LastSeq, &r.At); err != nil {
			return nil, err
		}
		r.Title = title.String
		r.Preview = centerSnippet(r.Preview, query, previewChars)
		results = append(results, r)
	}
	return results, rows.Err()
}
