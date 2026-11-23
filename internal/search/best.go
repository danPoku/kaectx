// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package search

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/danPoku/kaectx/internal/embed"
	"github.com/danPoku/kaectx/internal/governance"
	"github.com/danPoku/kaectx/internal/redact"
	"github.com/danPoku/kaectx/internal/store"
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
	// ProjectID and ProjectName are the hit's own project. They differ from
	// the caller's only when the search ran with Scope.AllProjects.
	ProjectID   int64
	ProjectName string
	// Reliability and PolicyScore are populated by opt-in risk-aware search.
	// GovernanceNeutral means there was too little outcome evidence to adjust
	// this result; its multiplier is exactly 1.
	Reliability         float64
	PolicyScore         float64
	GovernanceNeutral   bool
	GovernanceEvaluated bool
	OriginalRank        int
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
	// GovernanceApplied is true only when the opt-in policy was evaluated
	// successfully. GovernanceErr is fail-open and preserves the base order.
	GovernanceApplied bool
	GovernanceErr     error
}

// Best tries hybrid (keyword+semantic) search first, falling back to
// keyword-only on any failure. Semantic search is always an enhancement,
// never a requirement, matching the same "optional, log and continue"
// posture migrations/002_vectors.sql takes at the schema level: a machine
// without Ollama running, or before the embed worker has caught up, still
// gets full keyword search. client may be nil to skip straight to
// keyword-only (e.g. a caller that already knows embeddings aren't set up).
func Best(ctx context.Context, db *sql.DB, client embed.Embedder, query string, projectID int64, agent string, limit int) (BestResult, error) {
	return BestIn(ctx, db, client, query, projectID, agent, Window{}, limit)
}

// BestIn is Best limited to turns inside w.
func BestIn(ctx context.Context, db *sql.DB, client embed.Embedder, query string, projectID int64, agent string, w Window, limit int) (BestResult, error) {
	return BestScoped(ctx, db, client, query, Scope{ProjectID: projectID}, agent, w, limit)
}

// BestScoped is BestIn over a Scope: one project, or all of them.
func BestScoped(ctx context.Context, db *sql.DB, client embed.Embedder, query string, scope Scope, agent string, w Window, limit int) (BestResult, error) {
	var fallbackReason error
	if client != nil {
		hy, err := HybridScoped(ctx, db, client, query, scope, agent, w, limit)
		if err == nil {
			out := make([]Result, len(hy))
			for i, r := range hy {
				out[i] = Result{ChunkID: r.ChunkID, SessionID: r.SessionID, Agent: r.Agent, StartedAt: r.StartedAt, At: r.At, Snippet: r.Preview, FirstSeq: r.FirstSeq, LastSeq: r.LastSeq, Score: r.Score, ScoreKind: "rrf", ProjectID: r.ProjectID, ProjectName: r.ProjectName}
			}
			return BestResult{Results: out, UsedHybrid: true}, nil
		}
		fallbackReason = err
	}

	kw, err := KeywordScoped(db, query, scope, agent, w, limit)
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
	return BestTrackedWithPolicy(ctx, db, client, query, projectID, agent, limit, surface, false)
}

// BestTrackedWithPolicy optionally reranks results using explicit outcome
// observations. The original rank is the utility baseline; reliability can
// only discount it, and sparse evidence is exactly neutral.
func BestTrackedWithPolicy(ctx context.Context, db *sql.DB, client embed.Embedder, query string, projectID int64, agent string, limit int, surface string, riskAware bool) (BestResult, error) {
	return BestTrackedIn(ctx, db, client, query, projectID, agent, Window{}, limit, surface, riskAware)
}

// BestTrackedIn is BestTrackedWithPolicy limited to turns inside w.
func BestTrackedIn(ctx context.Context, db *sql.DB, client embed.Embedder, query string, projectID int64, agent string, w Window, limit int, surface string, riskAware bool) (BestResult, error) {
	return BestTrackedScoped(ctx, db, client, query, Scope{ProjectID: projectID}, agent, w, limit, surface, riskAware)
}

// BestTrackedScoped is BestTrackedIn over a Scope. With Scope.AllProjects the
// run is still recorded under the caller's project, and each candidate stores
// the project it actually came from, so a later read or label can be tied to
// the right one.
func BestTrackedScoped(ctx context.Context, db *sql.DB, client embed.Embedder, query string, scope Scope, agent string, w Window, limit int, surface string, riskAware bool) (BestResult, error) {
	projectID := scope.ProjectID
	if limit <= 0 {
		limit = 10
	}
	started := time.Now()
	got, err := BestScoped(ctx, db, client, query, scope, agent, w, limit)
	if err != nil {
		return got, err
	}
	if riskAware {
		got.GovernanceErr = applyGovernance(ctx, db, projectID, got.Results)
		got.GovernanceApplied = got.GovernanceErr == nil
	}
	telemetryCtx, cancel := context.WithTimeout(context.Background(), telemetryWriteTimeout)
	defer cancel()
	got.RunID, got.TelemetryErr = recordRetrieval(telemetryCtx, db, query, projectID, agent, limit, surface, time.Since(started), got)
	if got.TelemetryErr != nil {
		// recordRetrieval returns its run ID alongside a failed commit, but
		// then no such run exists. Callers offer RunID for labelling, so it
		// must name a stored run or nothing.
		got.RunID = ""
	}
	return got, nil
}

// applyGovernance reranks results by reliability. Each hit is assessed
// against its OWN project's observations: under an all-projects search the
// hits are grouped by project and each group assessed separately, so a label
// recorded in one project can never move another project's evidence, and
// nothing (observations, counts, the prior) is pooled. Like a claims record,
// which follows the policy it was made on, not the branch that looked it up.
//
// A hit therefore gets exactly the reliability a project-scoped search in its
// own project would give it. fallbackProject covers results that carry no
// project of their own.
func applyGovernance(ctx context.Context, db *sql.DB, fallbackProject int64, results []Result) error {
	keys := make([]string, len(results))
	byProject := map[int64][]governance.Evidence{}
	for i, r := range results {
		keys[i] = evidenceKey(r)
		project := r.ProjectID
		if project == 0 {
			project = fallbackProject
		}
		byProject[project] = append(byProject[project],
			governance.Evidence{Key: keys[i], SessionID: r.SessionID, FirstSeq: r.FirstSeq, LastSeq: r.LastSeq})
	}
	// Evidence keys are built from the session ID, and a session belongs to
	// one project, so the per-project maps cannot collide when merged.
	assessments := make(map[string]governance.Assessment, len(results))
	for project, evidence := range byProject {
		got, err := governance.AssessMany(ctx, db, project, evidence)
		if err != nil {
			return err
		}
		for k, a := range got {
			assessments[k] = a
		}
	}
	// Build a complete copy first: assessment failures must leave both order
	// and metadata untouched so fail-open telemetry remains truthful.
	evaluated := append([]Result(nil), results...)
	for i := range evaluated {
		a := assessments[keys[i]]
		evaluated[i].Reliability = a.Reliability
		evaluated[i].GovernanceNeutral = a.Neutral
		evaluated[i].GovernanceEvaluated = true
		evaluated[i].OriginalRank = i + 1
		// A positive rank-derived utility is comparable across BM25 and RRF,
		// whose raw score directions and scales differ.
		evaluated[i].PolicyScore = a.Reliability / float64(60+i+1)
	}
	sort.SliceStable(evaluated, func(i, j int) bool {
		return evaluated[i].PolicyScore > evaluated[j].PolicyScore
	})
	copy(results, evaluated)
	return nil
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
			(run_id, rank, evidence_key, chunk_id, session_id, first_seq, last_seq, score, score_kind, original_rank, policy_applied, reliability, policy_score, governance_neutral, project_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, runID, i+1, evidenceKey(r), nullableChunkID(r.ChunkID), r.SessionID, r.FirstSeq, r.LastSeq, r.Score, r.ScoreKind, nullableOriginalRank(r), r.GovernanceEvaluated, nullableReliability(r), nullablePolicyScore(r), nullableNeutral(r), nullableProjectID(r.ProjectID)); err != nil {
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

// nullableProjectID stores a hit's project, or NULL when the hit carries
// none. A NULL reads as "the run's own project", which is also what every
// candidate recorded before the column existed means.
func nullableProjectID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullableChunkID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullableOriginalRank(r Result) any {
	if !r.GovernanceEvaluated {
		return nil
	}
	return r.OriginalRank
}

func nullableReliability(r Result) any {
	if !r.GovernanceEvaluated {
		return nil
	}
	return r.Reliability
}

func nullablePolicyScore(r Result) any {
	if !r.GovernanceEvaluated {
		return nil
	}
	return r.PolicyScore
}

func nullableNeutral(r Result) any {
	if !r.GovernanceEvaluated {
		return nil
	}
	return r.GovernanceNeutral
}

func newRunID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate retrieval run id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func evidenceKey(r Result) string {
	return store.EvidenceKey(r.SessionID, r.FirstSeq, r.LastSeq)
}

func toResults(kw []KeywordResult) []Result {
	out := make([]Result, len(kw))
	for i, r := range kw {
		out[i] = Result{ChunkID: r.ChunkID, SessionID: r.SessionID, Agent: r.Agent, StartedAt: r.StartedAt, At: r.At, Snippet: r.Snippet, FirstSeq: r.FirstSeq, LastSeq: r.LastSeq, Score: r.BM25, ScoreKind: "bm25", ProjectID: r.ProjectID, ProjectName: r.ProjectName}
	}
	return out
}
