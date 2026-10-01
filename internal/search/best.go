// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package search

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/danPoku/kaectx/internal/embed"
	"github.com/danPoku/kaectx/internal/redact"
)

const (
	telemetryWriteTimeout   = 500 * time.Millisecond
	retrievalRunsPerProject = 10000
	retentionCheckEvery     = 100
)

// Result is search's unified hit shape, produced by either Keyword or
// Hybrid — callers that just want "the best available search" don't need
// to know which one actually ran.
type Result struct {
	ChunkID   int64
	SessionID string
	Agent     string
	StartedAt string // when the SESSION started
	At        string // when this chunk's turn happened; use this for display
	Snippet   string
	FirstSeq  int // message range of the matching chunk; pass to get_session
	LastSeq   int
	Score     float64
	ScoreKind string
}

// BestResult reports what ran, not just the hits — callers (ctx search,
// the search_context MCP tool) can tell the user semantic search quietly
// degraded instead of presenting a fallback as if it were the real thing.
type BestResult struct {
	Results    []Result
	UsedHybrid bool
	// FallbackReason is non-nil iff a client was provided but hybrid search
	// couldn't run — no embedding worker has caught up yet, vec0 isn't
	// available on this machine, or the local Ollama isn't reachable.
	FallbackReason error
	// RunID identifies the append-only telemetry row written by BestTracked.
	// It is empty for callers that deliberately use untracked Best.
	RunID string
	// TelemetryErr never changes the search outcome. Retrieval is the product;
	// measurement is an observer and must fail open.
	TelemetryErr error
}

// Best tries hybrid (keyword+semantic) search first, falling back to
// keyword-only on any failure. Semantic search is always an enhancement,
// never a requirement, matching the same "optional, log and continue"
// posture migrations/002_vectors.sql takes at the schema level: a machine
// without Ollama running, or before the embed worker has caught up, still
// gets full keyword search. client may be nil to skip straight to
// keyword-only (e.g. a caller that already knows embeddings aren't set up).
func Best(ctx context.Context, db *sql.DB, client embed.Embedder, query string, projectID int64, agent string, limit int) (BestResult, error) {
	var fallbackReason error
	if client != nil {
		hy, err := Hybrid(ctx, db, client, query, projectID, agent, limit)
		if err == nil {
			out := make([]Result, len(hy))
			for i, r := range hy {
				out[i] = Result{ChunkID: r.ChunkID, SessionID: r.SessionID, Agent: r.Agent, StartedAt: r.StartedAt, At: r.At, Snippet: r.Preview, FirstSeq: r.FirstSeq, LastSeq: r.LastSeq, Score: r.Score, ScoreKind: "rrf"}
			}
			return BestResult{Results: out, UsedHybrid: true}, nil
		}
		fallbackReason = err
	}

	kw, err := Keyword(db, query, projectID, agent, limit)
	if err != nil {
		return BestResult{}, err
	}
	return BestResult{Results: toResults(kw), FallbackReason: fallbackReason}, nil
}

// BestTracked runs Best and records the returned ordering for later retrieval
// evaluation. Recording is intentionally best-effort: a locked telemetry table
// or a future migration problem must never turn a successful search into a
// failed one.
func BestTracked(ctx context.Context, db *sql.DB, client embed.Embedder, query string, projectID int64, agent string, limit int, surface string) (BestResult, error) {
	if limit <= 0 {
		limit = 10
	}
	started := time.Now()
	got, err := Best(ctx, db, client, query, projectID, agent, limit)
	if err != nil {
		return got, err
	}
	telemetryCtx, cancel := context.WithTimeout(context.Background(), telemetryWriteTimeout)
	defer cancel()
	got.RunID, got.TelemetryErr = recordRetrieval(telemetryCtx, db, query, projectID, agent, limit, surface, time.Since(started), got)
	return got, nil
}

func recordRetrieval(ctx context.Context, db *sql.DB, query string, projectID int64, agent string, limit int, surface string, elapsed time.Duration, got BestResult) (string, error) {
	runID, err := newRunID()
	if err != nil {
		return "", err
	}
	mode := "keyword"
	if got.UsedHybrid {
		mode = "hybrid"
	}
	var fallback any
	if got.FallbackReason != nil {
		fallback = redact.Redact(got.FallbackReason.Error())
	}
	var agentFilter any
	if agent != "" {
		agentFilter = agent
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO retrieval_runs
		(id, project_id, surface, query, agent_filter, requested_limit, mode, fallback_reason, duration_ms, result_count)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, runID, projectID, surface, redact.Redact(query), agentFilter, limit, mode, fallback, elapsed.Milliseconds(), len(got.Results)); err != nil {
		return "", err
	}
	for i, r := range got.Results {
		if _, err := tx.ExecContext(ctx, `INSERT INTO retrieval_candidates
			(run_id, rank, evidence_key, chunk_id, session_id, first_seq, last_seq, score, score_kind)
			VALUES (?,?,?,?,?,?,?,?,?)`, runID, i+1, evidenceKey(r), r.ChunkID, r.SessionID, r.FirstSeq, r.LastSeq, r.Score, r.ScoreKind); err != nil {
			return "", err
		}
	}
	// Telemetry is diagnostic evidence, not durable memory. Check retention in
	// batches rather than scanning on every search. rowid is monotonic within
	// this table, so the run inserted above cannot lose a timestamp tie against
	// an older random id.
	var runCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM retrieval_runs WHERE project_id = ?`, projectID).Scan(&runCount); err != nil {
		return "", err
	}
	if runCount > retrievalRunsPerProject && runCount%retentionCheckEvery == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM retrieval_runs
			WHERE project_id = ? AND rowid NOT IN (
				SELECT rowid FROM retrieval_runs WHERE project_id = ?
				ORDER BY rowid DESC LIMIT ?
			)`, projectID, projectID, retrievalRunsPerProject); err != nil {
			return "", err
		}
	}
	return runID, tx.Commit()
}

func newRunID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate retrieval run id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func evidenceKey(r Result) string {
	s := r.SessionID + "\x00" + strconv.Itoa(r.FirstSeq) + "\x00" + strconv.Itoa(r.LastSeq)
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func toResults(kw []KeywordResult) []Result {
	out := make([]Result, len(kw))
	for i, r := range kw {
		out[i] = Result{ChunkID: r.ChunkID, SessionID: r.SessionID, Agent: r.Agent, StartedAt: r.StartedAt, At: r.At, Snippet: r.Snippet, FirstSeq: r.FirstSeq, LastSeq: r.LastSeq, Score: r.BM25, ScoreKind: "bm25"}
	}
	return out
}
