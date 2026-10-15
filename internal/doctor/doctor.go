// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package doctor reports the health and observability of a ctx project.
package doctor

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/danPoku/kaectx/internal/feedback"
	"github.com/danPoku/kaectx/internal/governance"
)

type Report struct {
	ProjectName          string
	Sessions             int
	Messages             int
	Chunks               int
	PendingEmbeddings    int
	UnassignedChunks     int
	Sources              int
	SourcesWithErrors    int
	LastIngestedAt       sql.NullString
	RecentRetrievals     int
	RecentZeroResultRuns int
	RecentHybridRuns     int
	RecentPolicyRuns     int
	RecentOpenedResults  int
	RecentInferredOpens  int // opened only by a chunk_id read that get_chunk attributed to a search
	RecentLabelledOpens  int
	Outcomes             int
	RetrievedEvidence    int
	ObservedEvidence     int
	CredibleEvidence     int
	// SearchOutcomes and SearchReasons count verdicts on whole searches
	// recorded in the last seven days, keyed by feedback.SearchOutcomes and
	// feedback.SearchReasons.
	SearchOutcomes map[string]int
	SearchReasons  map[string]int
}

func (r Report) OutcomeCoverage() float64 {
	if r.RetrievedEvidence == 0 {
		return 0
	}
	return 100 * float64(r.ObservedEvidence) / float64(r.RetrievedEvidence)
}

func (r Report) NeutralObservedEvidence() int {
	return r.ObservedEvidence - r.CredibleEvidence
}

// Collect reads a consistent diagnostic snapshot. It never mutates or repairs
// the store: doctor is safe to run while the daemon or MCP server is active.
func Collect(ctx context.Context, db *sql.DB, projectID int64) (Report, error) {
	var r Report
	if err := db.QueryRowContext(ctx, `SELECT name FROM projects WHERE id=?`, projectID).Scan(&r.ProjectName); err != nil {
		return r, fmt.Errorf("read project: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM sessions WHERE project_id=?),
		(SELECT COUNT(*) FROM messages m JOIN sessions se ON se.id=m.session_id WHERE se.project_id=?),
		(SELECT COUNT(*) FROM chunks c JOIN sessions se ON se.id=c.session_id WHERE se.project_id=?),
		(SELECT COUNT(*) FROM chunks c JOIN sessions se ON se.id=c.session_id WHERE se.project_id=? AND c.embedded_at IS NULL)`,
		projectID, projectID, projectID, projectID).Scan(&r.Sessions, &r.Messages, &r.Chunks, &r.PendingEmbeddings); err != nil {
		return r, fmt.Errorf("read corpus health: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks c JOIN sessions se ON se.id=c.session_id WHERE se.project_id IS NULL`).Scan(&r.UnassignedChunks); err != nil {
		return r, fmt.Errorf("read unassigned chunks: %w", err)
	}
	// Source files are global because a parse failure may happen before a
	// session exists, leaving no honest project attribution.
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN last_error IS NOT NULL AND last_error<>'' THEN 1 ELSE 0 END),0),
		MAX(last_ingested_at)
		FROM sources`).Scan(&r.Sources, &r.SourcesWithErrors, &r.LastIngestedAt); err != nil {
		return r, fmt.Errorf("read ingestion health: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN result_count=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN mode='hybrid' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN EXISTS (
			SELECT 1 FROM retrieval_candidates rc WHERE rc.run_id=rr.id AND rc.policy_applied=1
		) THEN 1 ELSE 0 END),0)
		FROM retrieval_runs rr
		WHERE project_id=? AND created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-7 days')`, projectID).
		Scan(&r.RecentRetrievals, &r.RecentZeroResultRuns, &r.RecentHybridRuns, &r.RecentPolicyRuns); err != nil {
		return r, fmt.Errorf("read retrieval health: %w", err)
	}
	// The funnel reads open events, so a result counts if any open landed in
	// the window, not only its last one. A result opened both ways in the
	// window counts as exact: the exact open alone is enough to attribute it.
	if err := db.QueryRowContext(ctx, `WITH opened AS (
		SELECT e.run_id, e.rank, MIN(e.inferred) AS inferred_only
		FROM retrieval_open_events e JOIN retrieval_runs rr ON rr.id=e.run_id
		WHERE rr.project_id=? AND e.opened_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-7 days')
		GROUP BY e.run_id, e.rank
	)
	SELECT COUNT(*),
		COALESCE(SUM(inferred_only),0),
		COALESCE(SUM(CASE WHEN EXISTS (
			SELECT 1 FROM memory_observations mo
			WHERE mo.run_id=o.run_id AND mo.candidate_rank=o.rank
		) THEN 1 ELSE 0 END),0)
		FROM opened o`, projectID).
		Scan(&r.RecentOpenedResults, &r.RecentInferredOpens, &r.RecentLabelledOpens); err != nil {
		return r, fmt.Errorf("read retrieval feedback funnel: %w", err)
	}
	// Coverage counts labels by governance's own matching rule, so a label
	// that follows its exchange across rechunking counts here too.
	if err := db.QueryRowContext(ctx, `WITH c(k, s, f, l) AS (
		SELECT rc.evidence_key, MIN(rc.session_id), MIN(rc.first_seq), MIN(rc.last_seq)
		FROM retrieval_candidates rc JOIN retrieval_runs rr ON rr.id=rc.run_id
		WHERE rr.project_id=? GROUP BY rc.evidence_key
	), observed AS (
		SELECT c.k, COUNT(*) AS n FROM c JOIN memory_observations mo ON `+governance.MatchObservationsSQL()+`
		GROUP BY c.k
	)
	SELECT
		(SELECT COUNT(*) FROM memory_observations WHERE project_id=?),
		(SELECT COUNT(*) FROM c),
		(SELECT COUNT(*) FROM observed),
		(SELECT COUNT(*) FROM observed WHERE n>=?)`,
		projectID, projectID, projectID, governance.MinCredibleObservations).Scan(&r.Outcomes, &r.RetrievedEvidence, &r.ObservedEvidence, &r.CredibleEvidence); err != nil {
		return r, fmt.Errorf("read outcome coverage: %w", err)
	}
	if err := collectSearchOutcomes(ctx, db, projectID, &r); err != nil {
		return r, fmt.Errorf("read search outcomes: %w", err)
	}
	return r, nil
}

// collectSearchOutcomes counts the last seven days' verdicts on whole
// searches by outcome and by reason. Every known key starts at zero so the
// report always shows the full set, including gaps nobody has hit.
func collectSearchOutcomes(ctx context.Context, db *sql.DB, projectID int64, r *Report) error {
	r.SearchOutcomes = map[string]int{}
	for _, o := range feedback.SearchOutcomes {
		r.SearchOutcomes[o] = 0
	}
	r.SearchReasons = map[string]int{}
	for _, reason := range feedback.SearchReasons {
		r.SearchReasons[reason] = 0
	}
	rows, err := db.QueryContext(ctx, `SELECT outcome, COALESCE(reason,''), COUNT(*)
		FROM retrieval_run_observations
		WHERE project_id=? AND created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-7 days')
		GROUP BY outcome, reason`, projectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var outcome, reason string
		var n int
		if err := rows.Scan(&outcome, &reason, &n); err != nil {
			return err
		}
		r.SearchOutcomes[outcome] += n
		if reason != "" {
			r.SearchReasons[reason] += n
		}
	}
	return rows.Err()
}
